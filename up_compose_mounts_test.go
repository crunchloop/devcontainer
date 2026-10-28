package devcontainer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/compose"
	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/runtime"
)

// Compose-source devcontainers must carry cfg.Mounts of every type
// onto the primary service, as the image path's buildRunSpec does.
// Before this, composeBindMounts kept binds only, so a feature's
// volume mount — docker-in-docker's /var/lib/docker — was dropped and
// the inner dockerd ran on overlay-on-overlay, failing `docker run`.

// dindCfg resolves a config the way Up does: user mounts plus feature
// metadata merged with ${devcontainerId} substitution.
func dindCfg(t *testing.T) *config.ResolvedConfig {
	t.Helper()
	cfg := &config.ResolvedConfig{
		DevcontainerID:           "abc123",
		LocalWorkspaceFolder:     t.TempDir(),
		ContainerWorkspaceFolder: "/workspaces/test",
		Mounts: []config.Mount{
			{Type: config.MountBind, Source: "/host/cache", Target: "/cache", ReadOnly: true},
			{Type: config.MountTmpfs, Target: "/scratch"},
		},
	}
	config.MergeMetadata(cfg, config.SubstitutionContext{
		LocalWorkspaceFolder: cfg.LocalWorkspaceFolder,
		DevcontainerID:       cfg.DevcontainerID,
	}, []config.FeatureMetadata{{
		ID: "docker-in-docker",
		Mounts: []config.Mount{{
			Type:   config.MountVolume,
			Source: "dind-var-lib-docker-${devcontainerId}",
			Target: "/var/lib/docker",
		}},
	}})
	return cfg
}

// mountsRunOverride builds the mount part of the run override exactly
// as createFreshCompose does.
func mountsRunOverride(cfg *config.ResolvedConfig, opts UpOptions) compose.Override {
	return compose.Override{
		Service:         "app",
		ExtraBindMounts: composeBindMounts(cfg, opts),
		ExtraMounts:     composeMounts(cfg, opts),
	}
}

func TestComposeMounts_CarriesVolumeAndTmpfsFromMetadata(t *testing.T) {
	cfg := dindCfg(t)
	opts := UpOptions{ExtraMounts: []runtime.MountSpec{
		{Type: runtime.MountVolume, Source: "extra-vol", Target: "/extra"},
		{Type: runtime.MountBind, Source: "/host/extra", Target: "/extra-bind"},
	}}

	got := composeMounts(cfg, opts)
	// cfg.Mounts order after MergeMetadata: feature mounts, then the
	// user's.
	want := []compose.Mount{
		{Type: composetypes.VolumeTypeVolume, Source: "dind-var-lib-docker-abc123", Target: "/var/lib/docker"},
		{Type: composetypes.VolumeTypeTmpfs, Target: "/scratch"},
		{Type: composetypes.VolumeTypeVolume, Source: "extra-vol", Target: "/extra"},
	}
	if len(got) != len(want) {
		t.Fatalf("composeMounts = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("composeMounts[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Binds are unchanged: workspace, cfg bind, extra bind — nothing else.
	binds := composeBindMounts(cfg, opts)
	wantBinds := []compose.BindMount{
		{Source: cfg.LocalWorkspaceFolder, Target: "/workspaces/test"},
		{Source: "/host/cache", Target: "/cache", ReadOnly: true},
		{Source: "/host/extra", Target: "/extra-bind"},
	}
	if len(binds) != len(wantBinds) {
		t.Fatalf("composeBindMounts = %+v, want %+v", binds, wantBinds)
	}
	for i := range wantBinds {
		if binds[i] != wantBinds[i] {
			t.Errorf("composeBindMounts[%d] = %+v, want %+v", i, binds[i], wantBinds[i])
		}
	}
}

// volumeRecorder is the native-backend fake with CreateVolume recorded.
type volumeRecorder struct {
	*composeFake
	volumes []runtime.VolumeSpec
}

func (v *volumeRecorder) CreateVolume(ctx context.Context, spec runtime.VolumeSpec) (string, error) {
	v.volumes = append(v.volumes, spec)
	return spec.Name, nil
}

func TestUpComposeNative_CarriesFeatureVolumeMount(t *testing.T) {
	rec := &buildRecorder{fakeRuntime: newFakeRuntime()}
	rt := &volumeRecorder{composeFake: &composeFake{buildRecorder: rec}}
	eng, err := New(EngineOptions{Runtime: rt, ComposeBackend: ComposeBackendNative})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := dindCfg(t)
	opts := UpOptions{LocalEnv: map[string]string{}}
	opts.bus = newEventBus(events.NewEmitter(nil), nil)

	project := sidecarProject(map[string]composetypes.ServiceConfig{"app": {Image: "app:dev"}})
	project.Name = "dc-test"
	if _, err := eng.upComposeNative(context.Background(), cfg, opts, project,
		&config.ComposeSource{Service: "app"}, "dc-test", t.TempDir(), "app:dev",
		mountsRunOverride(cfg, opts), false); err != nil {
		t.Fatalf("upComposeNative: %v", err)
	}

	want := map[string]runtime.MountSpec{
		"/workspaces/test": {Type: runtime.MountBind, Source: cfg.LocalWorkspaceFolder},
		"/cache":           {Type: runtime.MountBind, Source: "/host/cache", ReadOnly: true},
		"/scratch":         {Type: runtime.MountTmpfs},
		"/var/lib/docker":  {Type: runtime.MountVolume, Source: "dc-test_dind-var-lib-docker-abc123"},
	}
	mounts := rec.createdSpec.Mounts
	if len(mounts) != len(want) {
		t.Fatalf("RunSpec.Mounts = %+v, want %d entries", mounts, len(want))
	}
	for _, m := range mounts {
		w, ok := want[m.Target]
		if !ok || m.Type != w.Type || m.Source != w.Source || m.ReadOnly != w.ReadOnly {
			t.Errorf("mount %+v, want %+v", m, w)
		}
	}
	if len(rt.volumes) != 1 || rt.volumes[0].Name != "dc-test_dind-var-lib-docker-abc123" {
		t.Errorf("CreateVolume calls = %+v, want the dind volume pre-created", rt.volumes)
	}
}

// shelloutRecorder is a ComposeRuntime that loads the files it is
// handed through compose-go — the merge `docker compose -f ... -f ...`
// performs — and stops the Up there.
type shelloutRecorder struct {
	*fakeRuntime
	project *composetypes.Project
}

var errStopAfterComposeUp = errors.New("stop after ComposeUp")

func (s *shelloutRecorder) ComposeUp(ctx context.Context, spec runtime.ComposeUpSpec, _ chan<- runtime.BuildEvent) error {
	p, err := compose.Load(ctx, compose.LoadOptions{
		Files: spec.Files, WorkingDir: spec.WorkingDir, ProjectName: spec.ProjectName,
	})
	if err != nil {
		return err
	}
	s.project = p
	return errStopAfterComposeUp
}

func (s *shelloutRecorder) ComposeDown(context.Context, runtime.ComposeDownSpec) error { return nil }

func (s *shelloutRecorder) ComposeContainerID(context.Context, runtime.ComposePsSpec, string) (string, error) {
	return "", nil
}

func TestUpComposeShellout_CarriesFeatureVolumeMount(t *testing.T) {
	rt := &shelloutRecorder{fakeRuntime: newFakeRuntime()}
	eng, err := New(EngineOptions{Runtime: rt, ComposeBackend: ComposeBackendShellout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := dindCfg(t)
	opts := UpOptions{LocalEnv: map[string]string{}}
	opts.bus = newEventBus(events.NewEmitter(nil), nil)

	dir := t.TempDir()
	userFile := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(userFile, []byte("services:\n  app:\n    image: app:dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := compose.Load(context.Background(), compose.LoadOptions{
		Files: []string{userFile}, WorkingDir: dir, ProjectName: "dc-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = eng.upComposeShellout(context.Background(), cfg, opts, project,
		&config.ComposeSource{Service: "app", Files: []string{userFile}}, "dc-test", dir, "app:dev",
		mountsRunOverride(cfg, opts), false)
	if !errors.Is(err, errStopAfterComposeUp) {
		t.Fatalf("upComposeShellout: %v, want the recorder's stop", err)
	}

	svc := rt.project.Services["app"]
	byTarget := map[string]composetypes.ServiceVolumeConfig{}
	for _, v := range svc.Volumes {
		byTarget[v.Target] = v
	}
	if v := byTarget["/var/lib/docker"]; v.Type != composetypes.VolumeTypeVolume || v.Source != "dind-var-lib-docker-abc123" {
		t.Errorf("/var/lib/docker = %+v, want the dind volume", v)
	}
	if v := byTarget["/scratch"]; v.Type != composetypes.VolumeTypeTmpfs {
		t.Errorf("/scratch = %+v, want tmpfs", v)
	}
	if v := byTarget["/cache"]; v.Type != composetypes.VolumeTypeBind || v.Source != "/host/cache" || !v.ReadOnly {
		t.Errorf("/cache = %+v, want the read-only bind", v)
	}
	if v := byTarget["/workspaces/test"]; v.Type != composetypes.VolumeTypeBind {
		t.Errorf("workspace = %+v, want bind", v)
	}
	if vol, ok := rt.project.Volumes["dind-var-lib-docker-abc123"]; !ok || vol.Name != "dc-test_dind-var-lib-docker-abc123" {
		t.Errorf("project volume = %+v (declared=%v), want dc-test_dind-var-lib-docker-abc123", vol, ok)
	}
}
