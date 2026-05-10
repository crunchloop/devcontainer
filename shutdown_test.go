package devcontainer

import (
	"context"
	"testing"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

func newTestWorkspace(action config.ShutdownAction, compose bool) *Workspace {
	labels := map[string]string{}
	if compose {
		labels["com.docker.compose.project"] = "dc-test"
	}
	return &Workspace{
		ID: "test",
		Config: &config.ResolvedConfig{
			ShutdownAction: action,
		},
		Container: &runtime.ContainerDetails{
			Container: runtime.Container{
				ID:    "container-1",
				Name:  "test",
				State: runtime.StateRunning,
			},
			Labels: labels,
		},
	}
}

func TestShutdown_NoneIsNoop(t *testing.T) {
	rt := newFakeRuntime()
	rt.containersByID["container-1"] = &runtime.ContainerDetails{
		Container: runtime.Container{ID: "container-1", State: runtime.StateRunning},
	}
	eng := &Engine{runtime: rt}

	if err := eng.Shutdown(context.Background(), newTestWorkspace(config.ShutdownNone, false)); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if len(rt.stoppedIDs) != 0 {
		t.Errorf("expected no stops for ShutdownNone, got %v", rt.stoppedIDs)
	}
	if len(rt.removedIDs) != 0 {
		t.Errorf("expected no removes for ShutdownNone, got %v", rt.removedIDs)
	}
}

func TestShutdown_StopContainerStops(t *testing.T) {
	rt := newFakeRuntime()
	rt.containersByID["container-1"] = &runtime.ContainerDetails{
		Container: runtime.Container{ID: "container-1", State: runtime.StateRunning},
	}
	eng := &Engine{runtime: rt}

	if err := eng.Shutdown(context.Background(), newTestWorkspace(config.ShutdownStopContainer, false)); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if len(rt.stoppedIDs) != 1 || rt.stoppedIDs[0] != "container-1" {
		t.Errorf("expected stop on container-1, got %v", rt.stoppedIDs)
	}
	if len(rt.removedIDs) != 0 {
		t.Errorf("Shutdown must not remove, got %v", rt.removedIDs)
	}
}

func TestShutdown_DefaultStopsImageWorkspace(t *testing.T) {
	// Empty ShutdownAction → spec default is "stop the container" for
	// non-compose workspaces.
	rt := newFakeRuntime()
	rt.containersByID["container-1"] = &runtime.ContainerDetails{
		Container: runtime.Container{ID: "container-1", State: runtime.StateRunning},
	}
	eng := &Engine{runtime: rt}

	if err := eng.Shutdown(context.Background(), newTestWorkspace("", false)); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if len(rt.stoppedIDs) != 1 {
		t.Errorf("expected default stop, got %v", rt.stoppedIDs)
	}
}

func TestShutdown_StopComposeStopsPrimary(t *testing.T) {
	rt := newFakeRuntime()
	rt.containersByID["container-1"] = &runtime.ContainerDetails{
		Container: runtime.Container{ID: "container-1", State: runtime.StateRunning},
	}
	eng := &Engine{runtime: rt}

	if err := eng.Shutdown(context.Background(), newTestWorkspace(config.ShutdownStopCompose, true)); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if len(rt.stoppedIDs) != 1 {
		t.Errorf("expected primary stop for stopCompose (no project Stop yet), got %v", rt.stoppedIDs)
	}
}

func TestShutdown_StopComposeOnNonComposeFallsBack(t *testing.T) {
	rt := newFakeRuntime()
	rt.containersByID["container-1"] = &runtime.ContainerDetails{
		Container: runtime.Container{ID: "container-1", State: runtime.StateRunning},
	}
	eng := &Engine{runtime: rt}

	if err := eng.Shutdown(context.Background(), newTestWorkspace(config.ShutdownStopCompose, false)); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if len(rt.stoppedIDs) != 1 {
		t.Errorf("expected single-container stop fallback, got %v", rt.stoppedIDs)
	}
}

func TestShutdown_NilWorkspaceRejected(t *testing.T) {
	eng := &Engine{runtime: newFakeRuntime()}
	if err := eng.Shutdown(context.Background(), nil); err == nil {
		t.Error("expected error on nil workspace")
	}
}

func TestShutdown_NilContainerRejected(t *testing.T) {
	eng := &Engine{runtime: newFakeRuntime()}
	ws := &Workspace{Config: &config.ResolvedConfig{ShutdownAction: config.ShutdownStopContainer}}
	if err := eng.Shutdown(context.Background(), ws); err == nil {
		t.Error("expected error on workspace with nil Container")
	}
}

func TestShutdown_EmptyContainerIDRejected(t *testing.T) {
	eng := &Engine{runtime: newFakeRuntime()}
	ws := &Workspace{
		Config:    &config.ResolvedConfig{ShutdownAction: config.ShutdownStopContainer},
		Container: &runtime.ContainerDetails{},
	}
	if err := eng.Shutdown(context.Background(), ws); err == nil {
		t.Error("expected error on workspace with empty Container.ID")
	}
}
