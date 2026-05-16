//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// Engine.Up parity tests under ComposeBackendNative. Same fixture
// shape as TestComposeSource_FullFlow (shellout path); identical
// assertions. Two of these together establish "native is feature-
// complete on Docker" — a green run here is the gate to flipping
// the default backend (PR16) and deleting the shellout path (PR17).

func TestComposeSource_Native_FullFlow(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngineWith(t, devcontainer.EngineOptions{
		ComposeBackend: devcontainer.ComposeBackendNative,
	})
	defer rt.Close()

	ws := writeComposeWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
		SkipLifecycle:        true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{
			Remove:        true,
			RemoveVolumes: true,
		})
	}()

	if wsObj.Container == nil {
		t.Fatal("Workspace.Container is nil")
	}
	if got := wsObj.Container.Labels[devcontainer.LabelDevcontainerID]; got != string(wsObj.ID) {
		t.Errorf("dev.containers.id label = %q, want %q", got, wsObj.ID)
	}
	if _, ok := wsObj.Container.Labels["com.docker.compose.project"]; !ok {
		t.Errorf("compose project label missing; container.Labels = %v", wsObj.Container.Labels)
	}

	// Feature install ran in the primary service.
	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/etc/compose-feature-marker"},
	})
	if err != nil {
		t.Fatalf("Exec marker: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("compose-feature-marker missing: stderr=%q", res.Stderr)
	}
	if !strings.Contains(res.Stdout, "compose-feature-ran") {
		t.Errorf("compose-feature-marker contents = %q", res.Stdout)
	}

	// Feature containerEnv + user-declared env both visible.
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "echo $FEATURE_FLAG:$USER_DECLARED"},
	})
	if err != nil {
		t.Fatalf("Exec env: %v", err)
	}
	if !strings.Contains(res.Stdout, "ran:from-compose") {
		t.Errorf("expected feature + user env both visible, got %q", res.Stdout)
	}

	// Workspace bind mount applied.
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd:        []string{"pwd"},
		WorkingDir: wsObj.Config.ContainerWorkspaceFolder,
	})
	if err != nil {
		t.Fatalf("Exec pwd: %v", err)
	}
	if !strings.Contains(res.Stdout, wsObj.Config.ContainerWorkspaceFolder) {
		t.Errorf("pwd = %q, want containerWorkspaceFolder = %q",
			res.Stdout, wsObj.Config.ContainerWorkspaceFolder)
	}
}

func TestComposeSource_Native_DownRemovesProject(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngineWith(t, devcontainer.EngineOptions{
		ComposeBackend: devcontainer.ComposeBackendNative,
	})
	defer rt.Close()

	ws := writeComposeWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
		SkipLifecycle:        true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	if err := eng.Down(ctx, wsObj, devcontainer.DownOptions{
		Remove:        true,
		RemoveVolumes: true,
	}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if _, err := eng.Attach(ctx, wsObj.ID); err == nil {
		t.Errorf("Attach after Down(Remove) should fail; project still present?")
	}
}
