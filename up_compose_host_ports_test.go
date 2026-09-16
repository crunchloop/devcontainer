package devcontainer

import (
	"context"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/compose"
	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/runtime"
)

func hostPortEngine(t *testing.T, disablePublishing bool) (*Engine, *buildRecorder) {
	t.Helper()
	rt := &buildRecorder{fakeRuntime: newFakeRuntime()}
	eng, err := New(EngineOptions{
		Runtime:                   &composeFake{buildRecorder: rt},
		ComposeBackend:            ComposeBackendNative,
		DisableHostPortPublishing: disablePublishing,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng, rt
}

// upComposeNative's inputs, minimal but real: one service publishing a
// host port, plus the workspace bind the run override always carries.
func hostPortUp(t *testing.T, eng *Engine, out chan events.Event) {
	t.Helper()
	project := sidecarProject(map[string]composetypes.ServiceConfig{
		"app": {
			Image: "app:dev",
			Ports: []composetypes.ServicePortConfig{
				{Target: 8080, Published: "8080", Protocol: "tcp"},
			},
		},
	})
	cfg := &config.ResolvedConfig{
		DevcontainerID:           "dc-test",
		LocalWorkspaceFolder:     t.TempDir(),
		ContainerWorkspaceFolder: "/workspaces/test",
	}
	opts := UpOptions{LocalEnv: map[string]string{}}
	opts.bus = newEventBus(events.NewEmitter(nil), out)

	src := &config.ComposeSource{Service: "app"}
	runOverride := compose.Override{
		Service: "app",
		ExtraBindMounts: []compose.BindMount{
			{Source: cfg.LocalWorkspaceFolder, Target: cfg.ContainerWorkspaceFolder},
		},
	}
	if _, err := eng.upComposeNative(context.Background(), cfg, opts, project, src,
		"dc-test", t.TempDir(), "app:dev", runOverride); err != nil {
		t.Fatalf("upComposeNative: %v", err)
	}
}

// The consumer check: the option has to reach the RunSpec the
// orchestrator hands the backend, not just the project it reads.
func TestUpComposeNative_DisableHostPortPublishingDropsPublishes(t *testing.T) {
	eng, rt := hostPortEngine(t, true)
	out := make(chan events.Event, 8)

	hostPortUp(t, eng, out)

	if got := rt.createdSpec.Ports; len(got) != 0 {
		t.Errorf("RunSpec.Ports = %+v, want no host publish to reach the backend", got)
	}

	close(out)
	var warned *events.WarnEvent
	for ev := range out {
		if w, ok := ev.(events.WarnEvent); ok && w.Code == "compose_host_port_publish_skipped" {
			warned = &w
			break
		}
	}
	if warned == nil {
		t.Fatal("no compose_host_port_publish_skipped warning; a silent no-op is what this option must not be")
	}
	// The message has to point somewhere, not just report a removal.
	for _, want := range []string{`"app"`, "8080:8080/tcp", "app:8080"} {
		if !strings.Contains(warned.Message, want) {
			t.Errorf("warning %q does not mention %q", warned.Message, want)
		}
	}
}

// The drop is opt-in: without the option the publish still reaches the
// backend, which is what the CLI and any engine that owns its host
// depend on.
func TestUpComposeNative_PublishesByDefault(t *testing.T) {
	eng, rt := hostPortEngine(t, false)

	hostPortUp(t, eng, nil)

	ports := rt.createdSpec.Ports
	if len(ports) != 1 {
		t.Fatalf("RunSpec.Ports = %+v, want the declared 8080 publish", ports)
	}
	if ports[0].HostPort != "8080" || ports[0].ContainerPort != 8080 {
		t.Errorf("RunSpec.Ports[0] = %+v, want host 8080 -> container 8080", ports[0])
	}
}

// R2 path parity: the shellout backend cannot honor the option — it
// hands ports: to `docker compose` — so it refuses instead of running
// with the option quietly ignored. The guard is the first thing on
// that path, ahead of any work.
func TestUpComposeShellout_RefusesDisableHostPortPublishing(t *testing.T) {
	rt := &buildRecorder{fakeRuntime: newFakeRuntime()}
	eng, err := New(EngineOptions{Runtime: rt, DisableHostPortPublishing: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = eng.upComposeShellout(context.Background(), nil, sidecarUpOptions(), nil, nil,
		"dc-test", t.TempDir(), "app:dev", compose.Override{}, false)
	if err == nil {
		t.Fatal("upComposeShellout accepted DisableHostPortPublishing; it would publish on the daemon's host anyway")
	}
	if !strings.Contains(err.Error(), "ComposeBackendNative") {
		t.Errorf("error %q does not say which backend honors the option", err)
	}
}

// composeFake answers the orchestrator primitives fakeRuntime leaves
// unimplemented, so a compose Up runs end to end in a unit test:
// no pre-existing containers, a network that creates, an image that
// resolves to a digest.
type composeFake struct {
	*buildRecorder
}

func (c *composeFake) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) (string, error) {
	return "net-" + spec.Name, nil
}

func (c *composeFake) ListContainers(ctx context.Context, filter runtime.LabelFilter) ([]runtime.Container, error) {
	return nil, nil
}

func (c *composeFake) InspectImage(ctx context.Context, ref string) (*runtime.ImageDetails, error) {
	return &runtime.ImageDetails{ID: "sha256:" + ref}, nil
}
