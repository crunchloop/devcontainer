//go:build integration && linux

// End-to-end Engine-level checkpoint/restore against a live Podman.
//
// Unlike runtime/podman/integration_test.go — which drives the Runtime
// directly and proves *memory* resume — this exercises the engine path:
// Engine.Up → Engine.Checkpoint → Engine.Restore, and asserts the part
// that only exists at the engine level: Restore rebuilds a full
// *Workspace. The workspace id is recovered from the devcontainer label
// the archive preserves, the restored container is live, and Exec works
// through the reattached workspace (substituter bound to the live env,
// rootfs back from the archive). Memory resume is covered at the runtime
// level; here the contract under test is the reattach.
//
// Linux-only (Podman) and skipped unless PODMAN_SOCKET is set:
//
//	PODMAN_SOCKET=unix:///run/podman/podman.sock \
//	  go test -tags integration -run Podman -count=1 ./test/integration
package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/podman"
)

func newPodmanEngine(t *testing.T) (*devcontainer.Engine, *podman.Runtime) {
	t.Helper()
	socket := os.Getenv("PODMAN_SOCKET")
	if socket == "" {
		t.Skip("set PODMAN_SOCKET to run the live Podman engine integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt, err := podman.New(ctx, podman.Options{Socket: socket})
	if err != nil {
		t.Skipf("Podman service unavailable at %q: %v", socket, err)
	}
	if !rt.Capabilities().Checkpoint {
		t.Fatalf("Capabilities().Checkpoint is false — libpod API not reachable at %q", socket)
	}
	eng, err := devcontainer.New(devcontainer.EngineOptions{Runtime: rt})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng, rt
}

func TestPodmanEngine_CheckpointRestore_ReattachesWorkspace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}
	image := os.Getenv("PODMAN_TEST_IMAGE")
	if image == "" {
		image = "docker.io/library/alpine:3.20"
	}

	eng, rt := newPodmanEngine(t)

	ws := writeWorkspace(t, `{"image": "`+image+`", "containerEnv": {"CKPT_MARKER": "reattach-ok"}}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{LocalWorkspaceFolder: ws, Recreate: true})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	origID := wsObj.ID
	t.Cleanup(func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
	})

	// Drop a marker into the writable rootfs. Podman's --export bundles
	// the rootfs layer, so the marker must survive into the restored
	// container — a check that restore reconstructed the filesystem, not
	// just the process.
	if _, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{Cmd: []string{"sh", "-c", "echo persisted > /reattach-marker"}}); err != nil {
		t.Fatalf("Exec (write marker): %v", err)
	}

	arch := filepath.Join(t.TempDir(), "ckpt.tar")
	ref, err := eng.Checkpoint(ctx, wsObj, devcontainer.CheckpointOptions{ArchivePath: arch, StopAfter: true, TCPEstablished: true})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if ref.Size == 0 {
		t.Errorf("checkpoint archive is empty (%s)", ref.ArchivePath)
	}

	// Migration shape: the source is gone. Remove the original container so
	// the restore truly recreates from the archive (and so no stale
	// container lingers sharing the devcontainer-id label).
	if err := rt.RemoveContainer(ctx, wsObj.Container.ID, runtime.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("RemoveContainer (source): %v", err)
	}
	time.Sleep(2 * time.Second)

	restored, err := eng.Restore(ctx, devcontainer.RestoreOptions{ArchivePath: arch, TCPEstablished: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() {
		if restored != nil && restored.Container != nil {
			_ = rt.RemoveContainer(context.Background(), restored.Container.ID, runtime.RemoveOptions{Force: true})
		}
	})

	// --- the reattach contract ------------------------------------------
	if restored.ID != origID {
		t.Errorf("reattached workspace id = %q, want %q (must be recovered from the preserved %s label)",
			restored.ID, origID, devcontainer.LabelDevcontainerID)
	}
	if restored.Container == nil || restored.Container.ID == "" {
		t.Fatalf("restored workspace has no container")
	}
	if got := restored.Container.Labels[devcontainer.LabelDevcontainerID]; got != string(origID) {
		t.Errorf("restored container devcontainer-id label = %q, want %q", got, origID)
	}
	if restored.Container.State != runtime.StateRunning {
		t.Errorf("restored container state = %q, want running", restored.Container.State)
	}

	// Exec through the reattached workspace: proves the substituter is
	// bound to the live container and the rootfs returned with the marker.
	res, err := eng.Exec(ctx, restored, devcontainer.ExecOptions{Cmd: []string{"cat", "/reattach-marker"}})
	if err != nil {
		t.Fatalf("Exec (read marker) through reattached workspace: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "persisted") {
		t.Errorf("marker read exit=%d stdout=%q — rootfs/exec not reattached", res.ExitCode, res.Stdout)
	}
	t.Logf("engine reattach OK: id=%s container=%s archive=%d bytes", restored.ID, restored.Container.ID, ref.Size)
}
