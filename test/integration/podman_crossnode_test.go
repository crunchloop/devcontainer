//go:build integration && linux

// Cross-node checkpoint/restore: the relocated-pod case. A pod running
// Podman is reclaimed and its devcontainer must resume on a *different*
// node whose Podman store never saw the image. We model "different node"
// as two Podman stores (two hosts) that share only a file path for the
// archive — exactly the production shape: each pod talks to its OWN local
// Podman socket, and the archive travels via PVC/registry (here, a shared
// directory).
//
// This is a TWO-PHASE test, run once per machine, coordinated through a
// shared DCCKPT_XNODE_DIR (must be a path visible to both, e.g. an
// OrbStack /Users mount):
//
//	# on the SOURCE host (its own Podman store):
//	PODMAN_SOCKET=unix:///run/podman/podman.sock DCCKPT_XNODE_DIR=/Users/.../xnode \
//	  ./integration_arm64.test -test.run TestPodmanXNode_Checkpoint -test.v
//	# on the DESTINATION host (a DIFFERENT, fresh Podman store):
//	PODMAN_SOCKET=unix:///run/podman/podman.sock DCCKPT_XNODE_DIR=/Users/.../xnode \
//	  ./integration_arm64.test -test.run TestPodmanXNode_Restore -test.v
//
// The destination phase NEVER pulls the image: if restore succeeds and the
// image then exists in its store, the archive proved self-contained.
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
)

const xnodeMarker = "xnode-persisted"

func xnodeDir(t *testing.T) string {
	d := os.Getenv("DCCKPT_XNODE_DIR")
	if d == "" {
		t.Skip("set DCCKPT_XNODE_DIR (a path shared between both hosts) to run the cross-node test")
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatalf("mkdir xnode dir: %v", err)
	}
	return d
}

func xnodeImage() string {
	if v := os.Getenv("PODMAN_TEST_IMAGE"); v != "" {
		return v
	}
	return "docker.io/library/alpine:3.20"
}

// Phase 1 (SOURCE host): bring a devcontainer up, mark its rootfs, and
// checkpoint it to the shared archive dir. Records the container name so
// the destination phase can clear a name collision on rerun.
func TestPodmanXNode_Checkpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}
	dir := xnodeDir(t)
	eng, _ := newPodmanEngine(t)
	image := xnodeImage()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The workspace folder must live on the SHARED path: a devcontainer
	// binds LocalWorkspaceFolder into the container, and cross-node restore
	// fails unless that bind source exists on the destination host too
	// (checkpoint-restore.md §2.4). This models the consumer's workspace
	// PVC, reattached on the new node. (A machine-local temp dir here makes
	// restore fail with crun "error stat'ing <src>: No such file".)
	wsDir := filepath.Join(dir, "ws")
	if err := os.MkdirAll(filepath.Join(wsDir, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, ".devcontainer", "devcontainer.json"), []byte(`{"image": "`+image+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{LocalWorkspaceFolder: wsDir, Recreate: true})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	// Mark the writable rootfs — must survive into the archive and across hosts.
	if _, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{Cmd: []string{"sh", "-c", "echo " + xnodeMarker + " > /xnode-marker"}}); err != nil {
		t.Fatalf("Exec (write marker): %v", err)
	}

	if _, err := eng.Checkpoint(ctx, wsObj, devcontainer.CheckpointOptions{
		ArchivePath: filepath.Join(dir, "single.tar"), StopAfter: true, TCPEstablished: true,
	}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// Hand the container name to the destination phase (restore re-creates
	// it with the archived name; a stale one would collide on rerun).
	if err := os.WriteFile(filepath.Join(dir, "name.txt"), []byte(wsObj.Container.Name), 0o644); err != nil {
		t.Fatalf("write name.txt: %v", err)
	}
	t.Logf("cross-node checkpoint written: %s (container %q, workspace %q)", filepath.Join(dir, "single.tar"), wsObj.Container.Name, wsObj.ID)
}

// Phase 2 (DESTINATION host, a DIFFERENT/fresh Podman store): restore from
// the shared archive WITHOUT ever pulling the image, and assert the
// workspace reattaches, the rootfs marker survived, and the image is now
// present (populated from the archive) — i.e. the archive is self-contained
// and node-independent.
func TestPodmanXNode_Restore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}
	dir := xnodeDir(t)
	eng, rt := newPodmanEngine(t)
	image := xnodeImage()
	archive := filepath.Join(dir, "single.tar")
	if _, err := os.Stat(archive); err != nil {
		t.Skipf("no archive at %s — run TestPodmanXNode_Checkpoint on the source host first", archive)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Clear any prior restored container (rerun name collision) and the
	// image, so "image present after restore" genuinely means "from the
	// archive". We deliberately do NOT pull the image here.
	if b, err := os.ReadFile(filepath.Join(dir, "name.txt")); err == nil {
		if nm := strings.TrimSpace(string(b)); nm != "" {
			_ = rt.RemoveContainer(ctx, nm, runtime.RemoveOptions{Force: true})
		}
	}
	_ = rt.RemoveImage(ctx, image)

	restored, err := eng.Restore(ctx, devcontainer.RestoreOptions{ArchivePath: archive, TCPEstablished: true})
	if err != nil {
		t.Fatalf("Restore on fresh store: %v", err)
	}
	t.Cleanup(func() {
		if restored != nil && restored.Container != nil {
			_ = rt.RemoveContainer(context.Background(), restored.Container.ID, runtime.RemoveOptions{Force: true})
		}
	})

	if restored.ID == "" {
		t.Error("reattached workspace has empty id (devcontainer label not recovered from archive)")
	}
	if restored.Container == nil || restored.Container.State != runtime.StateRunning {
		t.Fatalf("restored container not running: %+v", restored.Container)
	}

	// Rootfs traveled: the marker we wrote on the source host is present.
	res, err := eng.Exec(ctx, restored, devcontainer.ExecOptions{Cmd: []string{"cat", "/xnode-marker"}})
	if err != nil {
		t.Fatalf("Exec (read marker): %v", err)
	}
	if !strings.Contains(res.Stdout, xnodeMarker) {
		t.Errorf("marker = %q, want %q — rootfs did not travel in the archive", res.Stdout, xnodeMarker)
	}

	// Self-contained: the image now exists in this store, though we never
	// pulled it — it was populated from the archive.
	if _, err := rt.InspectImage(ctx, image); err != nil {
		t.Errorf("image %q absent after restore (%v) — archive was not self-contained", image, err)
	}
	t.Logf("cross-node restore OK on fresh store: workspace=%q container=%s marker+image from archive", restored.ID, restored.Container.ID)
}
