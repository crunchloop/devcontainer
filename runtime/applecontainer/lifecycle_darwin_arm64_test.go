//go:build darwin && arm64

package applecontainer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/runtime"
)

// TestLifecycle_EndToEnd exercises the full PR-C surface:
//   Run → Start → Inspect (running) → Stop → Inspect (stopped) → Remove
//
// Validates the create/start split + the JSON wire shape +
// integration with PR-B's InspectContainer. Skips when the daemon is
// down.
func TestLifecycle_EndToEnd(t *testing.T) {
	rt := runtimeOrSkip(t)
	ctx := context.Background()

	const id = "ac-lifecycle-e2e"
	// Pre-clean leftover from a previous failed run.
	_ = rt.RemoveContainer(ctx, id, runtime.RemoveOptions{Force: true})
	t.Cleanup(func() {
		_ = rt.RemoveContainer(ctx, id, runtime.RemoveOptions{Force: true})
	})

	// Ensure alpine is locally cached (RunContainer requires it).
	cliRunStrict(t,
		"run", "--rm", "--name", "ac-alpine-warmup",
		"docker.io/library/alpine:latest", "/bin/true",
	)

	created, err := rt.RunContainer(ctx, runtime.RunSpec{
		Image: "docker.io/library/alpine:latest",
		Name:  id,
		Cmd:   []string{"sleep", "120"},
		Env:   map[string]string{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		Labels: map[string]string{
			"dev.containers.id":    "lifecycle-test-7",
			"dev.containers.engine": "devcontainer-go/test",
		},
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if created.ID != id {
		t.Errorf("created.ID: want %q got %q", id, created.ID)
	}
	if created.State != runtime.StateCreated {
		t.Errorf("created.State: want %q got %q", runtime.StateCreated, created.State)
	}

	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	// Give the apiserver a beat to flip status; the snapshot reflects
	// the latest known state of the runtime status finite-state machine.
	if err := waitForState(t, rt, id, runtime.StateRunning, 5*time.Second); err != nil {
		t.Fatalf("waiting for running: %v", err)
	}

	// Inspect should now see the running container with our labels.
	details, err := rt.InspectContainer(ctx, id)
	if err != nil {
		t.Fatalf("InspectContainer (running): %v", err)
	}
	if got := details.Labels["dev.containers.id"]; got != "lifecycle-test-7" {
		t.Errorf("Labels[dev.containers.id]: want %q got %q", "lifecycle-test-7", got)
	}

	// Idempotent start: calling again on a running container should
	// no-op (matches CLI behavior + Docker semantics).
	if err := rt.StartContainer(ctx, id); err != nil {
		t.Errorf("StartContainer (idempotent): %v", err)
	}

	if err := rt.StopContainer(ctx, id, runtime.StopOptions{Timeout: 3 * time.Second}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	if err := waitForState(t, rt, id, runtime.StateExited, 5*time.Second); err != nil {
		t.Fatalf("waiting for stopped: %v", err)
	}

	if err := rt.RemoveContainer(ctx, id, runtime.RemoveOptions{}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}

	// Post-remove inspect should now return ContainerNotFoundError.
	if _, err := rt.InspectContainer(ctx, id); err == nil {
		t.Error("InspectContainer after Remove: want error, got nil")
	} else {
		var nf *runtime.ContainerNotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("InspectContainer after Remove: want *ContainerNotFoundError, got %T: %v", err, err)
		}
	}
}

// TestRunContainer_MissingImage asserts the typed-error contract for
// the load-bearing case: a caller asks us to create a container against
// an image that hasn't been pulled.
func TestRunContainer_MissingImage(t *testing.T) {
	rt := runtimeOrSkip(t)
	_, err := rt.RunContainer(context.Background(), runtime.RunSpec{
		Image: "docker.io/library/does-not-exist-zzz:0",
		Name:  "ac-missing-image",
		Cmd:   []string{"/bin/true"},
	})
	if err == nil {
		t.Fatal("RunContainer: want error, got nil")
	}
	var nf *runtime.ImageNotFoundError
	if !errors.As(err, &nf) {
		t.Logf("err: %T %v", err, err)
		t.Fatalf("want *ImageNotFoundError, got %T", err)
	}
}

// TestRunContainer_BindMount creates a container with a virtiofs bind
// mount and verifies the inspect path round-trips it. Doesn't run the
// container — just exercises the mount-spec wiring.
func TestRunContainer_BindMount(t *testing.T) {
	rt := runtimeOrSkip(t)
	ctx := context.Background()
	const id = "ac-bindmount-test"
	_ = rt.RemoveContainer(ctx, id, runtime.RemoveOptions{Force: true})
	t.Cleanup(func() {
		_ = rt.RemoveContainer(ctx, id, runtime.RemoveOptions{Force: true})
	})

	hostDir := t.TempDir()

	cliRunStrict(t,
		"run", "--rm", "--name", "ac-alpine-warmup",
		"docker.io/library/alpine:latest", "/bin/true",
	)

	_, err := rt.RunContainer(ctx, runtime.RunSpec{
		Image: "docker.io/library/alpine:latest",
		Name:  id,
		Cmd:   []string{"sleep", "60"},
		Mounts: []runtime.MountSpec{
			{Type: runtime.MountBind, Source: hostDir, Target: "/mnt/work", ReadOnly: false},
		},
	})
	if err != nil {
		t.Fatalf("RunContainer with bind: %v", err)
	}

	details, err := rt.InspectContainer(ctx, id)
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	// Apple normalizes the source via URL(fileURLWithPath:).absolutePath(),
	// which canonically appends a trailing slash for directories. Match
	// with TrimRight to absorb that without becoming hostage to the
	// quirk in case it changes.
	var found bool
	for _, m := range details.Mounts {
		if m.Target == "/mnt/work" &&
			strings.TrimRight(m.Source, "/") == strings.TrimRight(hostDir, "/") {
			found = true
		}
	}
	if !found {
		t.Errorf("bind mount not round-tripped; details.Mounts=%+v want target=/mnt/work source=%q",
			details.Mounts, hostDir)
	}
}

// waitForState polls InspectContainer until the desired state is
// observed or the timeout fires. Apple's runtime status transitions
// asynchronously through the apiserver event loop.
func waitForState(t *testing.T, rt *Runtime, id string, want runtime.State, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d, err := rt.InspectContainer(context.Background(), id)
		if err == nil && d.State == want {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return errors.New("timeout waiting for state " + string(want))
}
