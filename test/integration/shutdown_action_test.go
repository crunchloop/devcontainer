//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
)

// TestShutdownAction_NoneLeavesContainerRunning verifies that
// Engine.Shutdown honors `shutdownAction: none` by leaving the
// container running. Engine.Down (the explicit teardown call) is
// unaffected and still stops + removes — that's tested in the
// existing TestImageSource_FullLifecycle teardown path.
func TestShutdownAction_NoneLeavesContainerRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{
		"image": "`+testImage+`",
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
	// Cleanup with explicit Down — Shutdown wouldn't tear it down here
	// since we set the action to "none".
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

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

// TestShutdownAction_StopContainerStops verifies the spec default for
// non-compose workspaces: Shutdown with `stopContainer` (or unset)
// stops the container without removing it.
func TestShutdownAction_StopContainerStops(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{
		"image": "`+testImage+`",
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
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	if err := eng.Shutdown(ctx, wsObj); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	details, err := rt.InspectContainer(ctx, wsObj.Container.ID)
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	// State should be exited; the container record itself must persist
	// (Shutdown does not remove).
	if details.State == runtime.StateRunning {
		t.Errorf("container still running after Shutdown(stopContainer)")
	}
}
