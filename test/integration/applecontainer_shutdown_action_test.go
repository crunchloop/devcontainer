//go:build integration && darwin && arm64

// Apple-container backend: shutdownAction semantics. Mirrors
// shutdown_action_test.go for the docker backend.

package integration

import (
	"context"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
)

func TestAppleContainer_ShutdownAction_NoneLeavesRunning(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newAppleContainerEngine(t)
	ws := writeWorkspace(t, `{
		"image": "docker.io/library/alpine:latest",
		"shutdownAction": "none"
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		// Force teardown — Shutdown is a no-op on "none".
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
	}()

	if err := eng.Shutdown(ctx, wsObj); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	details, err := rt.InspectContainer(ctx, wsObj.Container.ID)
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if details.State != runtime.StateRunning {
		t.Errorf("container state after Shutdown(none) = %q, want %q", details.State, runtime.StateRunning)
	}
}

func TestAppleContainer_ShutdownAction_StopContainerStops(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newAppleContainerEngine(t)
	ws := writeWorkspace(t, `{
		"image": "docker.io/library/alpine:latest",
		"shutdownAction": "stopContainer"
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
	}()

	if err := eng.Shutdown(ctx, wsObj); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Allow the apiserver a moment to flip status.
	for i := 0; i < 40; i++ {
		details, err := rt.InspectContainer(ctx, wsObj.Container.ID)
		if err == nil && details.State != runtime.StateRunning {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("container still running 4s after Shutdown(stopContainer)")
}
