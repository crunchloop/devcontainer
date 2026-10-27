package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/runtime"
)

// Tests for Override.ExtraMounts — the volume and tmpfs mounts from
// devcontainer.json and feature metadata (cfg.Mounts) — on both
// compose backends: ApplyRunOverride (native) and WriteRunOverride
// (shell-out). The motivating mount is docker-in-docker's
// /var/lib/docker volume; without it the inner dockerd's data root
// sits on overlay and `docker run` fails with EINVAL.

// dindMount is docker-in-docker's feature mount after
// ${devcontainerId} substitution.
var dindMount = Mount{
	Type:   composetypes.VolumeTypeVolume,
	Source: "dind-var-lib-docker-abc123",
	Target: "/var/lib/docker",
}

func mountsProject() *composetypes.Project {
	p := projectForApply()
	p.Name = "dc-x"
	return p
}

func volumeAt(vols []composetypes.ServiceVolumeConfig, target string) []composetypes.ServiceVolumeConfig {
	var out []composetypes.ServiceVolumeConfig
	for _, v := range vols {
		if v.Target == target {
			out = append(out, v)
		}
	}
	return out
}

// ---- native: ApplyRunOverride ---------------------------------------

func TestApplyRunOverride_FeatureVolumeMountAddsServiceAndProjectVolume(t *testing.T) {
	proj := mountsProject()
	if err := ApplyRunOverride(proj, "app", Override{
		Service:     "app",
		ExtraMounts: []Mount{dindMount},
	}); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}

	got := volumeAt(proj.Services["app"].Volumes, "/var/lib/docker")
	if len(got) != 1 {
		t.Fatalf("/var/lib/docker entries = %+v, want exactly 1", got)
	}
	if got[0].Type != composetypes.VolumeTypeVolume || got[0].Source != "dind-var-lib-docker-abc123" || got[0].ReadOnly {
		t.Errorf("service volume = %+v, want rw volume dind-var-lib-docker-abc123", got[0])
	}

	vol, ok := proj.Volumes["dind-var-lib-docker-abc123"]
	if !ok {
		t.Fatalf("project.Volumes = %+v, want a top-level declaration so ensureNamedVolumes creates it", proj.Volumes)
	}
	if vol.Name != "dc-x_dind-var-lib-docker-abc123" {
		t.Errorf("project volume Name = %q, want the loader-normalized dc-x_dind-var-lib-docker-abc123", vol.Name)
	}
}

func TestApplyRunOverride_TmpfsPassesThrough(t *testing.T) {
	proj := mountsProject()
	if err := ApplyRunOverride(proj, "app", Override{
		Service: "app",
		ExtraMounts: []Mount{{
			Type:      composetypes.VolumeTypeTmpfs,
			Target:    "/scratch",
			TmpfsSize: 64 << 20,
			TmpfsMode: 0o1777,
		}},
	}); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	got := volumeAt(proj.Services["app"].Volumes, "/scratch")
	if len(got) != 1 {
		t.Fatalf("/scratch entries = %+v, want 1", got)
	}
	v := got[0]
	if v.Type != composetypes.VolumeTypeTmpfs || v.Source != "" {
		t.Errorf("tmpfs entry = %+v, want type tmpfs with no source", v)
	}
	if v.Tmpfs == nil || v.Tmpfs.Size != 64<<20 || v.Tmpfs.Mode != 0o1777 {
		t.Errorf("tmpfs options = %+v, want size 64MiB mode 1777", v.Tmpfs)
	}
	if len(proj.Volumes) != 0 {
		t.Errorf("project.Volumes = %+v, want none for a tmpfs", proj.Volumes)
	}
}

func TestApplyRunOverride_VolumeOptions(t *testing.T) {
	proj := mountsProject()
	if err := ApplyRunOverride(proj, "app", Override{
		Service: "app",
		ExtraMounts: []Mount{
			{Type: composetypes.VolumeTypeVolume, Source: "cache", Target: "/cache", ReadOnly: true, NoCopy: true},
			{Type: composetypes.VolumeTypeVolume, Target: "/anon"},
		},
	}); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	vols := proj.Services["app"].Volumes
	ro := volumeAt(vols, "/cache")
	if len(ro) != 1 || !ro[0].ReadOnly || ro[0].Volume == nil || !ro[0].Volume.NoCopy {
		t.Errorf("/cache = %+v, want read-only nocopy volume", ro)
	}
	anon := volumeAt(vols, "/anon")
	if len(anon) != 1 || anon[0].Source != "" {
		t.Errorf("/anon = %+v, want anonymous volume", anon)
	}
	if _, ok := proj.Volumes[""]; ok {
		t.Error("anonymous volume must not get a top-level declaration")
	}
	if _, ok := proj.Volumes["cache"]; !ok {
		t.Error("named volume cache not declared")
	}
}

// Binds keep their existing path and shape when ExtraMounts are added.
func TestApplyRunOverride_BindsUnchangedByExtraMounts(t *testing.T) {
	binds := []BindMount{
		{Source: "/host/proj", Target: "/workspaces/proj"},
		{Source: "/host/ro", Target: "/ro", ReadOnly: true},
	}
	without := mountsProject()
	if err := ApplyRunOverride(without, "app", Override{Service: "app", ExtraBindMounts: binds}); err != nil {
		t.Fatal(err)
	}
	with := mountsProject()
	if err := ApplyRunOverride(with, "app", Override{Service: "app", ExtraBindMounts: binds, ExtraMounts: []Mount{dindMount}}); err != nil {
		t.Fatal(err)
	}
	a, b := without.Services["app"].Volumes, with.Services["app"].Volumes
	if len(b) != len(a)+1 {
		t.Fatalf("volumes with ExtraMounts = %+v, want the bind-only list plus one", b)
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Source != b[i].Source || a[i].Target != b[i].Target || a[i].ReadOnly != b[i].ReadOnly {
			t.Errorf("entry %d changed: %+v -> %+v", i, a[i], b[i])
		}
	}
	if b[len(b)-1].Target != "/var/lib/docker" {
		t.Errorf("volume mount not appended after the binds: %+v", b)
	}
}

// The user's compose already mounts something at the metadata target:
// the user's entry is kept, ours is not added, and no project volume
// is declared for the skipped mount.
func TestApplyRunOverride_UserTargetWinsOverMetadataMount(t *testing.T) {
	proj := mountsProject()
	svc := proj.Services["app"]
	svc.Volumes = append(svc.Volumes, composetypes.ServiceVolumeConfig{
		Type: composetypes.VolumeTypeVolume, Source: "my-docker", Target: "/var/lib/docker",
	})
	proj.Services["app"] = svc
	proj.Volumes = composetypes.Volumes{"my-docker": {Name: "dc-x_my-docker", Labels: composetypes.Labels{"user": "kept"}}}

	if err := ApplyRunOverride(proj, "app", Override{Service: "app", ExtraMounts: []Mount{dindMount}}); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	got := volumeAt(proj.Services["app"].Volumes, "/var/lib/docker")
	if len(got) != 1 || got[0].Source != "my-docker" {
		t.Errorf("/var/lib/docker = %+v, want only the user's my-docker entry", got)
	}
	if _, ok := proj.Volumes["dind-var-lib-docker-abc123"]; ok {
		t.Error("skipped metadata volume must not be declared")
	}
	if proj.Volumes["my-docker"].Labels["user"] != "kept" {
		t.Error("user's top-level volume declaration changed")
	}
}

// A top-level volume the user already declares under the same name is
// left exactly as declared (driver, labels, external).
func TestApplyRunOverride_KeepsUserTopLevelVolumeDeclaration(t *testing.T) {
	proj := mountsProject()
	proj.Volumes = composetypes.Volumes{
		"dind-var-lib-docker-abc123": {Name: "custom", Driver: "local", Labels: composetypes.Labels{"k": "v"}},
	}
	if err := ApplyRunOverride(proj, "app", Override{Service: "app", ExtraMounts: []Mount{dindMount}}); err != nil {
		t.Fatal(err)
	}
	vol := proj.Volumes["dind-var-lib-docker-abc123"]
	if vol.Name != "custom" || vol.Driver != "local" || vol.Labels["k"] != "v" {
		t.Errorf("user declaration overwritten: %+v", vol)
	}
}

func TestApplyRunOverride_RejectsUnsupportedMountType(t *testing.T) {
	for _, m := range []Mount{
		{Type: composetypes.VolumeTypeBind, Source: "/h", Target: "/c"},
		{Type: "", Source: "x", Target: "/c"},
		{Type: composetypes.VolumeTypeTmpfs, Source: "x", Target: "/c"},
		{Type: composetypes.VolumeTypeVolume, Source: "x"},
	} {
		proj := mountsProject()
		if err := ApplyRunOverride(proj, "app", Override{Service: "app", ExtraMounts: []Mount{m}}); err == nil {
			t.Errorf("ApplyRunOverride(%+v) = nil, want error", m)
		}
	}
}

// Round trip through the native orchestrator: the override's volume
// and tmpfs reach the RunSpec the backend receives (named volume with
// its project-scoped name), and the named volume is pre-created with
// the project labels `Down --volumes` uses.
func TestApplyRunOverride_MountsReachRunSpecAndCreateVolume(t *testing.T) {
	proj := mountsProject()
	if err := ApplyRunOverride(proj, "app", Override{
		Service: "app",
		ExtraBindMounts: []BindMount{
			{Source: "/host/proj", Target: "/workspaces/proj"},
		},
		ExtraMounts: []Mount{
			dindMount,
			{Type: composetypes.VolumeTypeTmpfs, Target: "/scratch"},
		},
	}); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	svc := proj.Services["app"]
	svc.Build = nil
	proj.Services["app"] = svc

	rt := newMockRuntime()
	var seen runtime.RunSpec
	rt.OnRunContainer = func(spec runtime.RunSpec) (*runtime.Container, error) {
		seen = spec
		return nil, nil
	}
	if _, err := NewOrchestrator(rt).Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	want := map[string]runtime.MountSpec{
		"/app":             {Type: runtime.MountBind, Source: "./code", Target: "/app"},
		"/workspaces/proj": {Type: runtime.MountBind, Source: "/host/proj", Target: "/workspaces/proj"},
		"/var/lib/docker":  {Type: runtime.MountVolume, Source: "dc-x_dind-var-lib-docker-abc123", Target: "/var/lib/docker"},
		"/scratch":         {Type: runtime.MountTmpfs, Source: "", Target: "/scratch"},
	}
	if len(seen.Mounts) != len(want) {
		t.Fatalf("RunSpec.Mounts = %+v, want %d entries", seen.Mounts, len(want))
	}
	for _, m := range seen.Mounts {
		w, ok := want[m.Target]
		if !ok || m.Type != w.Type || m.Source != w.Source || m.ReadOnly != w.ReadOnly {
			t.Errorf("mount %+v, want %+v", m, w)
		}
	}

	labels, ok := rt.volumes["dc-x_dind-var-lib-docker-abc123"]
	if !ok {
		t.Fatalf("CreateVolume not called for the dind volume; created %v", rt.volumes)
	}
	if labels[LabelComposeProject] != "dc-x" || labels["com.docker.compose.volume"] != "dind-var-lib-docker-abc123" {
		t.Errorf("volume labels = %v, want project + compose volume labels", labels)
	}
	if rt.createVolumeCalls != 1 {
		t.Errorf("CreateVolume calls = %d, want 1 (tmpfs and binds are not volumes)", rt.createVolumeCalls)
	}
}

// ---- shell-out: WriteRunOverride ------------------------------------

// loadWithOverride loads the user's compose file plus the generated
// run override through compose-go — the same merge `docker compose`
// applies to `-f user.yml -f dc-run.yaml`.
func loadWithOverride(t *testing.T, userYAML string, ov Override) (*composetypes.Project, string) {
	t.Helper()
	dir := t.TempDir()
	userFile := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(userFile, []byte(userYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	user, err := Load(context.Background(), LoadOptions{Files: []string{userFile}, WorkingDir: dir, ProjectName: "dc-x"})
	if err != nil {
		t.Fatalf("Load user: %v", err)
	}
	runFile := filepath.Join(dir, "dc-run.yaml")
	if err := WriteRunOverride(runFile, user, ov); err != nil {
		t.Fatalf("WriteRunOverride: %v", err)
	}
	body, _ := os.ReadFile(runFile)
	merged, err := Load(context.Background(), LoadOptions{Files: []string{userFile, runFile}, WorkingDir: dir, ProjectName: "dc-x"})
	if err != nil {
		t.Fatalf("Load merged: %v\n%s", err, body)
	}
	return merged, string(body)
}

const plainUserCompose = `
services:
  app:
    image: alpine
    volumes:
      - type: bind
        source: /user/host
        target: /user/container
`

func TestWriteRunOverride_FeatureVolumeMountAddsServiceAndProjectVolume(t *testing.T) {
	proj, body := loadWithOverride(t, plainUserCompose, Override{
		Service: "app",
		ExtraBindMounts: []BindMount{
			{Source: "/host/proj", Target: "/workspaces/proj"},
		},
		ExtraMounts: []Mount{dindMount},
	})
	got := volumeAt(proj.Services["app"].Volumes, "/var/lib/docker")
	if len(got) != 1 || got[0].Type != composetypes.VolumeTypeVolume || got[0].Source != "dind-var-lib-docker-abc123" {
		t.Fatalf("/var/lib/docker = %+v, want the dind volume\n%s", got, body)
	}
	vol, ok := proj.Volumes["dind-var-lib-docker-abc123"]
	if !ok {
		t.Fatalf("top-level volume missing; compose would refuse an undeclared named volume\n%s", body)
	}
	if vol.Name != "dc-x_dind-var-lib-docker-abc123" {
		t.Errorf("volume name = %q, want dc-x_dind-var-lib-docker-abc123", vol.Name)
	}
	// Binds untouched.
	if b := volumeAt(proj.Services["app"].Volumes, "/workspaces/proj"); len(b) != 1 || b[0].Type != composetypes.VolumeTypeBind || b[0].Source != "/host/proj" {
		t.Errorf("workspace bind = %+v", b)
	}
	if b := volumeAt(proj.Services["app"].Volumes, "/user/container"); len(b) != 1 || b[0].Source != "/user/host" {
		t.Errorf("user bind = %+v", b)
	}
}

func TestWriteRunOverride_TmpfsPassesThrough(t *testing.T) {
	proj, body := loadWithOverride(t, plainUserCompose, Override{
		Service: "app",
		ExtraMounts: []Mount{{
			Type: composetypes.VolumeTypeTmpfs, Target: "/scratch", TmpfsSize: 64 << 20, TmpfsMode: 0o1777,
		}},
	})
	got := volumeAt(proj.Services["app"].Volumes, "/scratch")
	if len(got) != 1 || got[0].Type != composetypes.VolumeTypeTmpfs || got[0].Source != "" {
		t.Fatalf("/scratch = %+v, want a tmpfs\n%s", got, body)
	}
	if got[0].Tmpfs == nil || got[0].Tmpfs.Size != 64<<20 || got[0].Tmpfs.Mode != 0o1777 {
		t.Errorf("tmpfs options = %+v\n%s", got[0].Tmpfs, body)
	}
	if strings.Contains(body, "\nvolumes:") {
		t.Errorf("tmpfs must not declare a top-level volume:\n%s", body)
	}
}

func TestWriteRunOverride_UserTargetWinsOverMetadataMount(t *testing.T) {
	proj, body := loadWithOverride(t, `
services:
  app:
    image: alpine
    volumes:
      - my-docker:/var/lib/docker
volumes:
  my-docker:
    labels:
      user: kept
`, Override{Service: "app", ExtraMounts: []Mount{dindMount}})
	got := volumeAt(proj.Services["app"].Volumes, "/var/lib/docker")
	if len(got) != 1 || got[0].Source != "my-docker" {
		t.Fatalf("/var/lib/docker = %+v, want only the user's my-docker\n%s", got, body)
	}
	if strings.Contains(body, "dind-var-lib-docker") {
		t.Errorf("skipped metadata mount leaked into the override:\n%s", body)
	}
	if proj.Volumes["my-docker"].Labels["user"] != "kept" {
		t.Errorf("user's volume declaration changed: %+v", proj.Volumes["my-docker"])
	}
}

func TestWriteRunOverride_KeepsUserTopLevelVolumeDeclaration(t *testing.T) {
	proj, body := loadWithOverride(t, `
services:
  app:
    image: alpine
volumes:
  dind-var-lib-docker-abc123:
    name: custom
    labels:
      k: v
`, Override{Service: "app", ExtraMounts: []Mount{dindMount}})
	vol := proj.Volumes["dind-var-lib-docker-abc123"]
	if vol.Name != "custom" || vol.Labels["k"] != "v" {
		t.Errorf("user declaration changed: %+v\n%s", vol, body)
	}
	if got := volumeAt(proj.Services["app"].Volumes, "/var/lib/docker"); len(got) != 1 {
		t.Errorf("/var/lib/docker = %+v, want the dind service volume", got)
	}
}

func TestWriteRunOverride_RejectsUnsupportedMountType(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dc-run.yaml")
	err := WriteRunOverride(dst, nil, Override{
		Service:     "app",
		ExtraMounts: []Mount{{Type: composetypes.VolumeTypeBind, Source: "/h", Target: "/c"}},
	})
	if err == nil {
		t.Fatal("want error for a bind in ExtraMounts")
	}
}
