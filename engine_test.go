package devcontainer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

// fakeRuntime is an in-memory Runtime used for engine unit tests. It
// records every call and lets the test inject canned responses for the
// few methods Up/Exec/Down actually exercise in M2.
type fakeRuntime struct {
	mu sync.Mutex

	containersByID    map[string]*runtime.ContainerDetails
	containersByLabel map[string]*runtime.Container
	imagesByRef       map[string]*runtime.ImageDetails

	createSeq    int
	pulled       []string
	pullErr      error
	createdSpec  *runtime.RunSpec
	createErr    error
	startErr     error
	startedIDs   []string
	stoppedIDs   []string
	removedIDs   []string
	execCalls    []runtime.ExecOptions
	execResult   runtime.ExecResult
	inspectErr   error
	failNextStop error
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		containersByID:    map[string]*runtime.ContainerDetails{},
		containersByLabel: map[string]*runtime.Container{},
		imagesByRef:       map[string]*runtime.ImageDetails{},
	}
}

func (f *fakeRuntime) BuildImage(ctx context.Context, spec runtime.BuildSpec, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	return runtime.ImageRef{}, runtime.ErrNotImplemented
}

func (f *fakeRuntime) PullImage(ctx context.Context, ref string, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	if f.pullErr != nil {
		return runtime.ImageRef{}, f.pullErr
	}
	return runtime.ImageRef{ID: "sha256:fake", Tags: []string{ref}}, nil
}

func (f *fakeRuntime) RunContainer(ctx context.Context, spec runtime.RunSpec) (*runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.createdSpec = &spec
	f.createSeq++
	id := fmt.Sprintf("container-%s-%d", spec.Name, f.createSeq)
	c := &runtime.Container{
		ID:    id,
		Name:  spec.Name,
		Image: spec.Image,
		State: runtime.StateCreated,
	}
	f.containersByID[id] = &runtime.ContainerDetails{
		Container: *c,
		Labels:    spec.Labels,
		Env:       []string{"HOME=/root", "PATH=/usr/bin"},
		User:      spec.User,
		Mounts: []runtime.MountInspect{
			{Type: "bind", Source: spec.Mounts[0].Source, Target: spec.Mounts[0].Target},
		},
	}
	if id := spec.Labels[LabelDevcontainerID]; id != "" {
		f.containersByLabel[id] = c
	}
	return c, nil
}

func (f *fakeRuntime) StartContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.startedIDs = append(f.startedIDs, id)
	if d, ok := f.containersByID[id]; ok {
		d.State = runtime.StateRunning
	}
	return nil
}

func (f *fakeRuntime) StopContainer(ctx context.Context, id string, opts runtime.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextStop != nil {
		err := f.failNextStop
		f.failNextStop = nil
		return err
	}
	f.stoppedIDs = append(f.stoppedIDs, id)
	if d, ok := f.containersByID[id]; ok {
		d.State = runtime.StateExited
	}
	return nil
}

func (f *fakeRuntime) RemoveContainer(ctx context.Context, id string, opts runtime.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedIDs = append(f.removedIDs, id)
	if d, ok := f.containersByID[id]; ok {
		for k, v := range f.containersByLabel {
			if v.ID == id {
				delete(f.containersByLabel, k)
			}
		}
		_ = d
		delete(f.containersByID, id)
	}
	return nil
}

func (f *fakeRuntime) ExecContainer(ctx context.Context, id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, opts)
	return f.execResult, nil
}

func (f *fakeRuntime) InspectContainer(ctx context.Context, id string) (*runtime.ContainerDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	d, ok := f.containersByID[id]
	if !ok {
		return nil, &runtime.ContainerNotFoundError{ID: id}
	}
	return d, nil
}

func (f *fakeRuntime) ContainerLogs(ctx context.Context, id string, w io.Writer, follow bool) error {
	return nil
}

func (f *fakeRuntime) InspectImage(ctx context.Context, ref string) (*runtime.ImageDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.imagesByRef[ref]; ok {
		return d, nil
	}
	return nil, &runtime.ImageNotFoundError{Ref: ref}
}

// ---- compose orchestrator primitives ---------------------------------
// The engine's image / build / compose-shellout paths never reach these,
// but the runtime.Runtime interface now declares them, so fakeRuntime
// must answer. ErrNotImplemented signals "not exercised in this test";
// any production code path that hits them through fakeRuntime would
// surface that error in the test.

func (f *fakeRuntime) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) (string, error) {
	return "", runtime.ErrNotImplemented
}

func (f *fakeRuntime) RemoveNetwork(ctx context.Context, id string) error {
	return runtime.ErrNotImplemented
}

func (f *fakeRuntime) CreateVolume(ctx context.Context, spec runtime.VolumeSpec) (string, error) {
	return "", runtime.ErrNotImplemented
}

func (f *fakeRuntime) RemoveVolume(ctx context.Context, name string) error {
	return runtime.ErrNotImplemented
}

func (f *fakeRuntime) ListContainers(ctx context.Context, filter runtime.LabelFilter) ([]runtime.Container, error) {
	return nil, runtime.ErrNotImplemented
}

func (f *fakeRuntime) ListImages(ctx context.Context, filter runtime.LabelFilter) ([]runtime.ImageRef, error) {
	return nil, runtime.ErrNotImplemented
}

func (f *fakeRuntime) RemoveImage(ctx context.Context, ref string) error {
	return runtime.ErrNotImplemented
}

func (f *fakeRuntime) FindContainerByLabel(ctx context.Context, key, value string) (*runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key != LabelDevcontainerID {
		return nil, nil
	}
	c, ok := f.containersByLabel[value]
	if !ok {
		return nil, nil
	}
	if d, ok := f.containersByID[c.ID]; ok {
		c.State = d.State
	}
	return c, nil
}

func writeImageDevcontainer(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	dc := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(dc, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dc, "devcontainer.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEngineNew_RuntimeRequired(t *testing.T) {
	if _, err := New(EngineOptions{}); err == nil {
		t.Fatal("expected error when Runtime is nil")
	}
}

func TestUp_ImageSource_FreshCreate(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)

	wsObj, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !reflect.DeepEqual(rt.pulled, []string{"alpine:3.20"}) {
		t.Errorf("pulled = %v", rt.pulled)
	}
	if rt.createdSpec == nil || rt.createdSpec.Image != "alpine:3.20" {
		t.Errorf("createdSpec = %+v", rt.createdSpec)
	}
	if rt.createdSpec.Labels[LabelDevcontainerID] != string(wsObj.ID) {
		t.Errorf("missing/incorrect devcontainer id label: %v", rt.createdSpec.Labels)
	}
	if len(rt.startedIDs) != 1 {
		t.Errorf("expected 1 start, got %d", len(rt.startedIDs))
	}
	if wsObj.Container.State != runtime.StateRunning {
		t.Errorf("container state = %q", wsObj.Container.State)
	}
}

func TestUp_BuildSourceInvokesBuildImage(t *testing.T) {
	// fakeRuntime returns runtime.ErrNotImplemented from BuildImage; this
	// asserts that Up actually reaches the build path for *BuildSource
	// (rather than short-circuiting with a "not implemented" of its own).
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{"build":{"dockerfile":"Dockerfile"}}`)

	_, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if !errors.Is(err, runtime.ErrNotImplemented) {
		t.Fatalf("expected ErrNotImplemented (from fake BuildImage), got %v", err)
	}
}

func TestUp_ComposeSourceErrors(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{"dockerComposeFile":"compose.yml","service":"app"}`)

	_, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if !errors.Is(err, runtime.ErrNotImplemented) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}

// TestUp_ComposeBackendNative_DispatchesToOrchestrator confirms that
// EngineOptions.ComposeBackend = ComposeBackendNative routes through
// the in-process orchestrator instead of asserting ComposeRuntime.
// fakeRuntime does NOT implement ComposeRuntime; on the shellout
// path the request would fail with ErrNotImplemented before any
// project load. On native, the request should proceed past the
// type-assertion check and surface a different failure mode (here:
// the orchestrator's RunContainer hits BuildImage → ErrNotImplemented).
// The point is the dispatch reached the native path at all.
func TestUp_ComposeBackendNative_DispatchesToOrchestrator(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{
		Runtime:        rt,
		ComposeBackend: ComposeBackendNative,
	})

	dir := t.TempDir()
	dc := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(dc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dc, "devcontainer.json"),
		[]byte(`{"dockerComposeFile":"compose.yml","service":"app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Minimal compose project so compose.Load succeeds.
	if err := os.WriteFile(filepath.Join(dc, "compose.yml"),
		[]byte("services:\n  app:\n    image: alpine:3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: dir})
	if err == nil {
		t.Fatal("expected an error from fakeRuntime hitting unimplemented primitive paths")
	}
	// The error must NOT be "runtime does not support compose" — that
	// would mean we dispatched to shellout. Native dispatch routes
	// past the ComposeRuntime assertion entirely.
	if strings.Contains(err.Error(), "runtime does not support compose") {
		t.Errorf("native dispatch fell through to shellout assertion: %v", err)
	}
}

func TestUp_ReattachStopped(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)

	// First Up creates the container.
	first, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("first Up: %v", err)
	}
	// Simulate stop (e.g. host reboot).
	if err := eng.Down(context.Background(), first, DownOptions{}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	rt.pulled = nil      // reset
	rt.createdSpec = nil // reset

	// Second Up should restart, not recreate.
	second, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if rt.createdSpec != nil {
		t.Errorf("expected re-attach, but RunContainer was called: %+v", rt.createdSpec)
	}
	if second.Container.ID != first.Container.ID {
		t.Errorf("container id changed across re-attach: %q vs %q", first.Container.ID, second.Container.ID)
	}
}

func TestUp_RecreateForcesFresh(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)

	first, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("first Up: %v", err)
	}

	second, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws, Recreate: true})
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if second.Container.ID == first.Container.ID {
		t.Errorf("expected fresh container id with Recreate, got same: %q", second.Container.ID)
	}
	if len(rt.removedIDs) == 0 {
		t.Error("Recreate should have removed the existing container")
	}
}

func TestUp_ExtraMountsAndContainerEnv(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image": "alpine:3.20",
		"containerEnv": { "FROM_CONFIG": "config", "OVERRIDE_ME": "config" },
		"mounts": [ "type=bind,source=/cfg-src,target=/cfg-tgt" ]
	}`)

	if _, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		ExtraMounts: []runtime.MountSpec{
			{Type: runtime.MountBind, Source: "/host/dap", Target: "/dap"},
		},
		ExtraContainerEnv: map[string]string{
			"FROM_EXTRA":  "extra",
			"OVERRIDE_ME": "extra",
		},
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if rt.createdSpec == nil {
		t.Fatal("createdSpec is nil")
	}

	// Mounts: workspace bind + cfg.Mounts entry + extra. Order:
	// workspace, then cfg.Mounts, then extras (we don't pin the
	// workspace target, just assert the cfg + extra entries are present).
	var sawCfg, sawExtra bool
	for _, m := range rt.createdSpec.Mounts {
		if m.Source == "/cfg-src" && m.Target == "/cfg-tgt" {
			sawCfg = true
		}
		if m.Source == "/host/dap" && m.Target == "/dap" {
			sawExtra = true
		}
	}
	if !sawCfg {
		t.Errorf("cfg.Mounts entry missing from RunSpec: %+v", rt.createdSpec.Mounts)
	}
	if !sawExtra {
		t.Errorf("ExtraMounts entry missing from RunSpec: %+v", rt.createdSpec.Mounts)
	}

	// Env: cfg entries preserved, extras merged on top, conflicts resolved
	// in favor of extras.
	if got := rt.createdSpec.Env["FROM_CONFIG"]; got != "config" {
		t.Errorf("FROM_CONFIG = %q, want %q", got, "config")
	}
	if got := rt.createdSpec.Env["FROM_EXTRA"]; got != "extra" {
		t.Errorf("FROM_EXTRA = %q, want %q", got, "extra")
	}
	if got := rt.createdSpec.Env["OVERRIDE_ME"]; got != "extra" {
		t.Errorf("OVERRIDE_ME = %q, want extras to win, got %q", got, "extra")
	}
}

func TestExec_SubstitutesContainerEnv(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)

	wsObj, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Up performs a userEnvProbe exec; clear the recorded calls so this
	// test only asserts on the user-driven Exec.
	rt.execCalls = nil
	rt.execResult = runtime.ExecResult{ExitCode: 0, Stdout: "ok"}
	_, err = eng.Exec(context.Background(), wsObj, ExecOptions{
		Cmd: []string{"echo", "${containerEnv:HOME}"},
		Env: map[string]string{"FOO": "${containerEnv:PATH}"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(rt.execCalls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(rt.execCalls))
	}
	got := rt.execCalls[0]
	if !reflect.DeepEqual(got.Cmd, []string{"echo", "/root"}) {
		t.Errorf("cmd not substituted: %v", got.Cmd)
	}
	if got.Env["FOO"] != "/usr/bin" {
		t.Errorf("env not substituted: %v", got.Env)
	}
}

func TestDown_RemoveCleansContainer(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)

	wsObj, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := eng.Down(context.Background(), wsObj, DownOptions{Remove: true}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if c, _ := rt.FindContainerByLabel(context.Background(), LabelDevcontainerID, string(wsObj.ID)); c != nil {
		t.Errorf("expected container removed, still found: %+v", c)
	}
}

func TestDown_NotFoundIsSuccess(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)

	wsObj, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	rt.failNextStop = &runtime.ContainerNotFoundError{ID: wsObj.Container.ID}
	if err := eng.Down(context.Background(), wsObj, DownOptions{}); err != nil {
		t.Errorf("Down should swallow NotFound, got %v", err)
	}
}

func TestEngine_CtxCancelled(t *testing.T) {
	rt := newFakeRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := eng.Up(ctx, UpOptions{LocalWorkspaceFolder: "/x"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected ctx.Canceled, got %v", err)
	}
}

func TestSubstituter_Slice(t *testing.T) {
	cfg := &config.ResolvedConfig{
		LocalWorkspaceFolder:     "/h/p",
		ContainerWorkspaceFolder: "/c/p",
		DevcontainerID:           "abc",
	}
	details := &runtime.ContainerDetails{Env: []string{"HOME=/root"}}
	s := newSubstituter(cfg, details, nil)
	got, _ := s.Slice([]string{"${localWorkspaceFolder}", "${containerEnv:HOME}", "literal"})
	want := []string{"/h/p", "/root", "literal"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestEnvListToMap(t *testing.T) {
	got := envListToMap([]string{"A=1", "B=2", "MALFORMED", "C=hello=world"})
	want := map[string]string{"A": "1", "B": "2", "C": "hello=world"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUp_PrebakedImageSkipsFeatureFetch(t *testing.T) {
	// Set up a fake runtime where the base image has a
	// devcontainer.metadata label declaring 'pre-baked-feature' is
	// already installed. Up should NOT call BuildImage for features.
	rt := newFakeRuntime()
	rt.imagesByRef["alpine:3.20"] = &runtime.ImageDetails{
		ID:   "sha256:base",
		Tags: []string{"alpine:3.20"},
		Labels: map[string]string{
			"devcontainer.metadata": `[{"id":"pre-baked-feature","version":"1.5.0"}]`,
		},
	}
	eng, _ := New(EngineOptions{Runtime: rt})

	// devcontainer.json requests pre-baked-feature@1 (compatible).
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"features": {
			"ghcr.io/x/pre-baked-feature:1": {"version":"1.0.0"}
		}
	}`)

	wsObj, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		SkipLifecycle:        true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	if len(wsObj.Config.Features) != 1 {
		t.Fatalf("Features count = %d", len(wsObj.Config.Features))
	}
	if !wsObj.Config.Features[0].AlreadyInstalled {
		t.Error("expected feature to be marked AlreadyInstalled from base label")
	}
	// Container should run from the base image directly — no
	// dc-go-final-* tag created. fakeRuntime would have errored on
	// BuildImage (returns ErrNotImplemented) if we'd tried.
	if wsObj.Container.Image != "alpine:3.20" {
		t.Errorf("container image = %q, want alpine:3.20 (no rebuild needed)", wsObj.Container.Image)
	}
}

func TestUp_PrebakedImage_StrictModeRejectsLooseMatch(t *testing.T) {
	// In strict mode, version 1.5.0 in the label does NOT satisfy a
	// request for 1.0.0 (which permissive mode would accept).
	rt := newFakeRuntime()
	rt.imagesByRef["alpine:3.20"] = &runtime.ImageDetails{
		ID:   "sha256:base",
		Tags: []string{"alpine:3.20"},
		Labels: map[string]string{
			"devcontainer.metadata": `[{"id":"foo","version":"1.5.0"}]`,
		},
	}
	eng, _ := New(EngineOptions{
		Runtime:                   rt,
		StrictFeatureVersionMatch: true,
	})

	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"features": { "ghcr.io/x/foo:1": {"version":"1.0.0"} }
	}`)

	_, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		SkipLifecycle:        true,
	})
	// In strict mode, the feature is NOT marked installed → fetch
	// kicks in → fakeRuntime returns ErrNotImplemented from build.
	// We expect an error.
	if err == nil {
		t.Fatal("expected error: feature should not be skipped in strict mode")
	}
}

func TestContainerName(t *testing.T) {
	if got := containerName("deadbeef"); !strings.HasPrefix(got, "devcontainer-") {
		t.Errorf("name = %q, want devcontainer- prefix", got)
	}
}
