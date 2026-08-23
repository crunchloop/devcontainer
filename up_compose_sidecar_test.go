package devcontainer

import (
	"context"
	"path/filepath"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/runtime"
)

// buildRecorder wraps fakeRuntime with a BuildImage that succeeds and
// records every spec, so sidecar-build tests can assert what was built
// without a real backend.
type buildRecorder struct {
	*fakeRuntime
	builds []runtime.BuildSpec
}

func (b *buildRecorder) BuildImage(ctx context.Context, spec runtime.BuildSpec, ch chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	b.builds = append(b.builds, spec)
	return runtime.ImageRef{ID: "sha256:" + spec.Tag, Tags: []string{spec.Tag}}, nil
}

func sidecarProject(services map[string]composetypes.ServiceConfig) *composetypes.Project {
	svcs := composetypes.Services{}
	for name, svc := range services {
		svc.Name = name
		svcs[name] = svc
	}
	return &composetypes.Project{Services: svcs}
}

func sidecarEngine(t *testing.T) (*Engine, *buildRecorder) {
	t.Helper()
	rt := &buildRecorder{fakeRuntime: newFakeRuntime()}
	eng, err := New(EngineOptions{Runtime: rt, ComposeBackend: ComposeBackendNative})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng, rt
}

func sidecarUpOptions() UpOptions {
	opts := UpOptions{}
	opts.bus = newEventBus(events.NewEmitter(nil), nil)
	return opts
}

// The shellout backend delegated sidecar `build:` services to
// `docker compose up`; the native orchestrator only creates containers
// from images, so the engine must build them first — a build-only
// sidecar previously reached ContainerCreate with an empty image.
func TestBuildComposeSidecarImages_BuildsNonPrimaryServices(t *testing.T) {
	eng, rt := sidecarEngine(t)
	workingDir := t.TempDir()

	project := sidecarProject(map[string]composetypes.ServiceConfig{
		// Primary: ApplyBuildOverride has already cleared Build by the
		// time the helper runs; simulate that state.
		"app": {Image: "dc-final:latest"},
		// Build-only sidecar → compose v2's default <project>-<service>.
		"db": {Build: &composetypes.BuildConfig{Context: "./db", Dockerfile: "Dockerfile"}},
		// image: + build: → the built image is tagged with image:.
		"worker": {Image: "acme/worker:dev", Build: &composetypes.BuildConfig{Context: "./worker"}},
		// Plain image sidecar → untouched.
		"cache": {Image: "redis:7"},
	})
	src := &config.ComposeSource{Service: "app"}

	if err := eng.buildComposeSidecarImages(context.Background(), project, src, "dc-x", workingDir, sidecarUpOptions()); err != nil {
		t.Fatalf("buildComposeSidecarImages: %v", err)
	}

	if len(rt.builds) != 2 {
		t.Fatalf("builds = %d, want 2 (db, worker); specs: %+v", len(rt.builds), rt.builds)
	}
	// Deterministic order: sorted by service name.
	if rt.builds[0].Tag != "dc-x-db" {
		t.Errorf("db build tag = %q, want dc-x-db", rt.builds[0].Tag)
	}
	if got, want := rt.builds[0].ContextPath, filepath.Join(workingDir, "db"); got != want {
		t.Errorf("db build context = %q, want %q", got, want)
	}
	if rt.builds[1].Tag != "acme/worker:dev" {
		t.Errorf("worker build tag = %q, want acme/worker:dev", rt.builds[1].Tag)
	}

	// The project must end up image-only so the orchestrator's hash,
	// pull-retry, and drift checks all see a concrete reference.
	for name, wantImage := range map[string]string{
		"db":     "dc-x-db",
		"worker": "acme/worker:dev",
		"cache":  "redis:7",
	} {
		svc := project.Services[name]
		if svc.Image != wantImage {
			t.Errorf("service %q image = %q, want %q", name, svc.Image, wantImage)
		}
		if svc.Build != nil {
			t.Errorf("service %q still carries build:", name)
		}
	}
}

// runServices restricts which services come up; services outside the
// selection must not be built either.
func TestBuildComposeSidecarImages_HonorsRunServices(t *testing.T) {
	eng, rt := sidecarEngine(t)

	project := sidecarProject(map[string]composetypes.ServiceConfig{
		"app":      {Image: "dc-final:latest"},
		"db":       {Build: &composetypes.BuildConfig{Context: "./db"}},
		"excluded": {Build: &composetypes.BuildConfig{Context: "./excluded"}},
	})
	src := &config.ComposeSource{Service: "app", RunServices: []string{"db"}}

	if err := eng.buildComposeSidecarImages(context.Background(), project, src, "dc-x", t.TempDir(), sidecarUpOptions()); err != nil {
		t.Fatalf("buildComposeSidecarImages: %v", err)
	}

	if len(rt.builds) != 1 || rt.builds[0].Tag != "dc-x-db" {
		t.Fatalf("builds = %+v, want exactly the selected db service", rt.builds)
	}
	if svc := project.Services["excluded"]; svc.Build == nil {
		t.Error("unselected service was mutated")
	}
}

// A dependency of a selected service must be built even when
// runServices doesn't name it directly — `docker compose up app`
// builds app's depends_on closure.
func TestBuildComposeSidecarImages_BuildsDependenciesOfSelection(t *testing.T) {
	eng, rt := sidecarEngine(t)

	project := sidecarProject(map[string]composetypes.ServiceConfig{
		"app": {Image: "dc-final:latest", DependsOn: composetypes.DependsOnConfig{
			"db": composetypes.ServiceDependency{Condition: "service_started"},
		}},
		"db":       {Build: &composetypes.BuildConfig{Context: "./db"}},
		"excluded": {Build: &composetypes.BuildConfig{Context: "./excluded"}},
	})
	src := &config.ComposeSource{Service: "app", RunServices: []string{"app"}}

	if err := eng.buildComposeSidecarImages(context.Background(), project, src, "dc-x", t.TempDir(), sidecarUpOptions()); err != nil {
		t.Fatalf("buildComposeSidecarImages: %v", err)
	}
	if len(rt.builds) != 1 || rt.builds[0].Tag != "dc-x-db" {
		t.Fatalf("builds = %+v, want app's dependency db and nothing else", rt.builds)
	}
}
