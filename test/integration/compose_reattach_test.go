//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/feature"
	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

// Compose reattach: an Up whose project containers already exist from
// identical inputs must restart them from the existing images, reading
// the feature metadata from the primary image's label — no feature
// fetch, no image build, so no registry. These tests serve a small
// feature from an in-process registry and cut the registry off before
// the second Up.

// blockableTransport makes the feature registry unreachable on demand,
// failing the way a guest without egress does (a refused connection).
type blockableTransport struct {
	blocked atomic.Bool
	calls   atomic.Int64
}

func (b *blockableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b.calls.Add(1)
	if b.blocked.Load() {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused (registry blocked by test)")}
	}
	return http.DefaultTransport.RoundTrip(req)
}

// countingStore counts Fetch calls on the wrapped store.
type countingStore struct {
	inner   feature.Store
	fetches atomic.Int64
}

func (c *countingStore) Fetch(ctx context.Context, ref string, kind config.FeatureSourceKind) (feature.Fetched, error) {
	c.fetches.Add(1)
	return c.inner.Fetch(ctx, ref, kind)
}

// reattachHarness is one engine with a blockable, counted feature
// store whose cache lives in the test's temp dir.
type reattachHarness struct {
	eng   *devcontainer.Engine
	rt    *docker.Runtime
	gate  *blockableTransport
	store *countingStore
}

func newReattachHarness(t *testing.T, backend devcontainer.ComposeBackend) *reattachHarness {
	t.Helper()
	gate := &blockableTransport{}
	ds, err := feature.NewDiskStore(feature.DiskStoreOptions{
		CacheDir:     t.TempDir(),
		OCIKeychain:  authn.NewMultiKeychain(authn.DefaultKeychain),
		OCITransport: gate,
	})
	if err != nil {
		t.Fatalf("NewDiskStore: %v", err)
	}
	store := &countingStore{inner: ds}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt, err := docker.New(ctx, docker.Options{})
	if err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	eng, err := devcontainer.New(devcontainer.EngineOptions{
		Runtime:        rt,
		FeatureStore:   store,
		ComposeBackend: backend,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &reattachHarness{eng: eng, rt: rt, gate: gate, store: store}
}

// block cuts the registry off and resets the counters.
func (h *reattachHarness) block() {
	h.gate.blocked.Store(true)
	h.reset()
}

func (h *reattachHarness) reset() {
	h.gate.calls.Store(0)
	h.store.fetches.Store(0)
}

// markerFeatureMetadata declares one of everything the compose
// run override carries from feature metadata: env, a capability, a
// project volume, an entrypoint and a lifecycle command.
const markerFeatureMetadata = `{
  "id": "marker",
  "version": "1.0.0",
  "options": {"flavor": {"type": "string", "default": "plain"}},
  "containerEnv": {"MARKER_FEATURE": "on"},
  "capAdd": ["SYS_PTRACE"],
  "mounts": [{"source": "marker-data-${devcontainerId}", "target": "/marker-data", "type": "volume"}],
  "entrypoint": "/usr/local/share/marker-entry.sh",
  "postStartCommand": "echo poststart >> /marker-data/poststart.log"
}`

const markerFeatureInstall = `#!/bin/sh
set -e
mkdir -p /usr/local/share
echo "$FLAVOR" > /usr/local/share/marker-flavor
cat > /usr/local/share/marker-entry.sh <<'EOF'
#!/bin/sh
mkdir -p /marker-data
echo entry >> /marker-data/entry.log
EOF
chmod +x /usr/local/share/marker-entry.sh
`

// serveMarkerFeature starts an in-process registry holding the marker
// feature and returns its reference.
func serveMarkerFeature(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{
		{"devcontainer-feature.json", markerFeatureMetadata},
		{"install.sh", markerFeatureInstall},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(
		mutate.ConfigMediaType(empty.Image, types.MediaType("application/vnd.devcontainers")),
		static.NewLayer(buf.Bytes(), "application/vnd.devcontainers.layer.v1+tar"))
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimPrefix(srv.URL, "http://") + "/features/marker:1.0.0"
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsed, img); err != nil {
		t.Fatalf("push marker feature: %v", err)
	}
	return ref
}

// writeMarkerWorkspace writes a compose devcontainer with one app
// service and a sidecar, using the marker feature with flavor.
func writeMarkerWorkspace(t *testing.T, dir, featureRef, flavor string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, "docker-compose.yml"), `
services:
  app:
    image: debian:bookworm-slim
    command: ["sh", "-c", "while sleep 1000; do :; done"]
  sidecar:
    image: busybox:1.36
    command: ["sh", "-c", "while sleep 1000; do :; done"]
`)
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"dockerComposeFile": "../docker-compose.yml",
		"service": "app",
		"workspaceFolder": "/workspaces/proj",
		"features": { "`+featureRef+`": { "flavor": "`+flavor+`" } }
	}`)
}

// eventLog collects every event of one Up.
type eventLog struct {
	ch   chan events.Event
	done chan struct{}
	mu   sync.Mutex
	evs  []events.Event
}

func newEventLog() *eventLog {
	l := &eventLog{ch: make(chan events.Event, 4096), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for ev := range l.ch {
			l.mu.Lock()
			l.evs = append(l.evs, ev)
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *eventLog) close() []events.Event {
	close(l.ch)
	<-l.done
	return l.evs
}

func featureBuilds(evs []events.Event) int {
	n := 0
	for _, ev := range evs {
		if b, ok := ev.(events.BuildStartEvent); ok && b.Source == events.BuildSourceFeatures {
			n++
		}
	}
	return n
}

func reattachSkips(evs []events.Event) int {
	n := 0
	for _, ev := range evs {
		if s, ok := ev.(events.FeatureSkippedEvent); ok && s.Reason == "compose_reattach" {
			n++
		}
	}
	return n
}

// up runs one Up and returns the workspace and its events.
func (h *reattachHarness) up(t *testing.T, ctx context.Context, dir string, opts devcontainer.UpOptions) (*devcontainer.Workspace, []events.Event) {
	t.Helper()
	l := newEventLog()
	opts.LocalWorkspaceFolder = dir
	opts.Events = l.ch
	ws, err := h.eng.Up(ctx, opts)
	evs := l.close()
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	return ws, evs
}

// stopProject stops every container of the workspace's project, as a
// host or VM stop leaves them.
func (h *reattachHarness) stopProject(t *testing.T, ctx context.Context, ws *devcontainer.Workspace) {
	t.Helper()
	project := ws.Container.Labels["com.docker.compose.project"]
	cs, err := h.rt.ListContainers(ctx, runtime.LabelFilter{Match: map[string]string{"com.docker.compose.project": project}})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(cs) < 2 {
		t.Fatalf("project %s has %d containers, want app and sidecar", project, len(cs))
	}
	for _, c := range cs {
		if err := h.rt.StopContainer(ctx, c.ID, runtime.StopOptions{Timeout: 5 * time.Second}); err != nil {
			t.Fatalf("stop %s: %v", c.Name, err)
		}
	}
}

// cleanupComposeWorkspace removes the project, its volumes and the
// images the engine tagged for it.
func cleanupComposeWorkspace(t *testing.T, eng *devcontainer.Engine, ws *devcontainer.Workspace) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		project := ws.Container.Labels["com.docker.compose.project"]
		_ = eng.Down(ctx, ws, devcontainer.DownOptions{Remove: true, RemoveVolumes: true})
		if out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output(); err == nil {
			for _, id := range strings.Fields(string(out)) {
				_ = exec.Command("docker", "rm", "-f", id).Run()
			}
		}
		if out, err := exec.Command("docker", "volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+project).Output(); err == nil {
			for _, v := range strings.Fields(string(out)) {
				_ = exec.Command("docker", "volume", "rm", "-f", v).Run()
			}
		}
		_ = exec.Command("docker", "network", "rm", project+"_default").Run()
		for _, prefix := range []string{"dc-go-final-", "dc-go-uid-", "dc-go-compose-base-"} {
			_ = exec.Command("docker", "image", "rm", "-f", prefix+string(ws.ID)+":latest").Run()
		}
	})
}

func execOut(t *testing.T, ctx context.Context, eng *devcontainer.Engine, ws *devcontainer.Workspace, cmd ...string) string {
	t.Helper()
	res, err := eng.Exec(ctx, ws, devcontainer.ExecOptions{Cmd: cmd})
	if err != nil {
		t.Fatalf("Exec %v: %v", cmd, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("Exec %v: exit %d stdout=%q stderr=%q", cmd, res.ExitCode, res.Stdout, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

var composeBackends = []struct {
	name    string
	backend devcontainer.ComposeBackend
}{
	{"native", devcontainer.ComposeBackendNative},
	{"shellout", devcontainer.ComposeBackendShellout},
}

// TestComposeReattach_RestartsWithoutRegistry: up, stop every
// service, cut the registry off, up again. The second Up must reattach
// — same containers, restarted, no fetch, no build — and still apply
// the feature's metadata from the image label: env, capability,
// volume, entrypoint and postStartCommand.
func TestComposeReattach_RestartsWithoutRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	featureRef := serveMarkerFeature(t)

	for _, bk := range composeBackends {
		for _, adopt := range []bool{false, true} {
			sub := bk.name
			if adopt {
				sub += "/adopt"
			}
			t.Run(sub, func(t *testing.T) {
				h := newReattachHarness(t, bk.backend)
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
				defer cancel()
				dir := t.TempDir()
				writeMarkerWorkspace(t, dir, featureRef, "plain")

				// First creation: unchanged — fetch, build, label.
				ws1, evs1 := h.up(t, ctx, dir, devcontainer.UpOptions{Recreate: true})
				cleanupComposeWorkspace(t, h.eng, ws1)
				if n := h.store.fetches.Load(); n != 1 {
					t.Fatalf("first Up: %d feature fetches, want 1", n)
				}
				if featureBuilds(evs1) != 1 {
					t.Fatalf("first Up: %d feature image builds, want 1", featureBuilds(evs1))
				}
				if ws1.Container.Labels[devcontainer.LabelComposeInputsHash] == "" {
					t.Fatalf("first Up: primary has no %s label: %v", devcontainer.LabelComposeInputsHash, ws1.Container.Labels)
				}
				if got := execOut(t, ctx, h.eng, ws1, "cat", "/marker-data/entry.log"); lineCount(got) != 1 {
					t.Fatalf("first Up: entry.log = %q, want one entry", got)
				}

				h.stopProject(t, ctx, ws1)
				h.block()

				ws2, evs2 := h.up(t, ctx, dir, devcontainer.UpOptions{AdoptExisting: adopt})
				if n := h.store.fetches.Load(); n != 0 {
					t.Errorf("reattach: %d feature fetches, want 0", n)
				}
				if n := h.gate.calls.Load(); n != 0 {
					t.Errorf("reattach: %d registry requests, want 0", n)
				}
				if n := featureBuilds(evs2); n != 0 {
					t.Errorf("reattach: %d feature image builds, want 0", n)
				}
				if n := reattachSkips(evs2); n != 1 {
					t.Errorf("reattach: %d compose_reattach skip events, want 1", n)
				}
				if ws2.Container.ID != ws1.Container.ID {
					t.Errorf("reattach recreated the primary: %s -> %s", ws1.Container.ID, ws2.Container.ID)
				}
				if ws2.Container.State != runtime.StateRunning {
					t.Errorf("primary state after reattach = %s, want running", ws2.Container.State)
				}
				cs, err := h.rt.ListContainers(ctx, runtime.LabelFilter{Match: map[string]string{
					"com.docker.compose.project": ws2.Container.Labels["com.docker.compose.project"],
					"com.docker.compose.service": "sidecar",
				}})
				if err != nil || len(cs) != 1 || cs[0].State != runtime.StateRunning {
					t.Errorf("sidecar after reattach = %+v (err %v), want one running", cs, err)
				}

				// Metadata from the image label reached cfg and the container.
				if ws2.Config.ContainerEnv["MARKER_FEATURE"] != "on" {
					t.Errorf("cfg.ContainerEnv = %v, want MARKER_FEATURE=on", ws2.Config.ContainerEnv)
				}
				if !containsStr(ws2.Config.CapAdd, "SYS_PTRACE") || !hasCapability(ws2.Container.CapAdd, "SYS_PTRACE") {
					t.Errorf("capAdd cfg=%v container=%v, want SYS_PTRACE", ws2.Config.CapAdd, ws2.Container.CapAdd)
				}
				var vol bool
				for _, m := range ws2.Container.Mounts {
					if m.Target == "/marker-data" && m.Type == "volume" {
						vol = true
					}
				}
				if !vol {
					t.Errorf("no volume at /marker-data after reattach: %+v", ws2.Container.Mounts)
				}
				if got := execOut(t, ctx, h.eng, ws2, "printenv", "MARKER_FEATURE"); got != "on" {
					t.Errorf("MARKER_FEATURE in container = %q", got)
				}
				// The entrypoint ran again on restart, and the lifecycle
				// command from the label ran on both Ups, into the same volume.
				if got := execOut(t, ctx, h.eng, ws2, "cat", "/marker-data/entry.log"); lineCount(got) != 2 {
					t.Errorf("entry.log after reattach = %q, want two entries", got)
				}
				if got := execOut(t, ctx, h.eng, ws2, "cat", "/marker-data/poststart.log"); lineCount(got) != 2 {
					t.Errorf("poststart.log after reattach = %q, want two entries", got)
				}
				if got := execOut(t, ctx, h.eng, ws2, "cat", "/usr/local/share/marker-flavor"); got != "plain" {
					t.Errorf("marker-flavor = %q, want plain", got)
				}
			})
		}
	}
}

// TestComposeReattach_ConfigChangeRebuilds: a changed feature option
// changes the inputs hash, so the next Up takes the full path — it
// fetches and builds again (needing the registry) rather than
// reattaching to an image built from other inputs.
func TestComposeReattach_ConfigChangeRebuilds(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	featureRef := serveMarkerFeature(t)

	for _, bk := range composeBackends {
		t.Run(bk.name, func(t *testing.T) {
			h := newReattachHarness(t, bk.backend)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			dir := t.TempDir()
			writeMarkerWorkspace(t, dir, featureRef, "plain")

			ws1, _ := h.up(t, ctx, dir, devcontainer.UpOptions{Recreate: true, SkipLifecycle: true})
			cleanupComposeWorkspace(t, h.eng, ws1)
			h.stopProject(t, ctx, ws1)

			writeMarkerWorkspace(t, dir, featureRef, "spicy")

			// Offline, the changed inputs must not be satisfied by the
			// old image: the full path runs and needs the registry. The
			// exact-version tag is served from the store's cache, so the
			// fetch succeeds; what matters is that it happens.
			h.block()
			ws2, evs := h.up(t, ctx, dir, devcontainer.UpOptions{SkipLifecycle: true})
			if n := h.store.fetches.Load(); n != 1 {
				t.Errorf("Up after config change: %d feature fetches, want 1", n)
			}
			if n := featureBuilds(evs); n != 1 {
				t.Errorf("Up after config change: %d feature image builds, want 1", n)
			}
			if n := reattachSkips(evs); n != 0 {
				t.Errorf("Up after config change reattached (%d skip events)", n)
			}
			if bk.backend == devcontainer.ComposeBackendNative {
				// Native without AdoptExisting recreates on drift.
				if ws2.Container.ID == ws1.Container.ID {
					t.Errorf("native: primary not recreated after config change")
				}
				if ws2.Container.Labels[devcontainer.LabelComposeInputsHash] == ws1.Container.Labels[devcontainer.LabelComposeInputsHash] {
					t.Errorf("native: inputs hash label unchanged after config change")
				}
				if got := execOut(t, ctx, h.eng, ws2, "cat", "/usr/local/share/marker-flavor"); got != "spicy" {
					t.Errorf("native: marker-flavor = %q, want spicy", got)
				}
			}
		})
	}
}
