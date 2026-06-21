package podman

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/runtime"
)

// TestIntegration_CheckpointRestore exercises the full Option-A path
// against a live Podman: the standard surface (pull/run/start/exec) via
// the embedded docker.Runtime over Podman's docker-compatible socket,
// plus Checkpoint/Restore via the libpod REST API on the same socket. It
// also stress-tests the transport-wedge concern (moby client + libpod
// calls against the same Podman store).
//
// Skipped unless PODMAN_SOCKET is set, e.g.:
//
//	PODMAN_SOCKET=unix:///run/podman/podman.sock \
//	  go test -run Integration -count=1 ./runtime/podman
func TestIntegration_CheckpointRestore(t *testing.T) {
	socket := os.Getenv("PODMAN_SOCKET")
	if socket == "" {
		t.Skip("set PODMAN_SOCKET to run the live Podman integration test")
	}
	image := os.Getenv("PODMAN_TEST_IMAGE")
	if image == "" {
		image = "docker.io/library/node:20-slim"
	}

	ctx := context.Background()
	rt, err := New(ctx, Options{Socket: socket})
	if err != nil {
		t.Fatalf("New(%q): %v", socket, err)
	}
	if !rt.Capabilities().Checkpoint {
		t.Fatalf("Capabilities().Checkpoint is false — podman/criu check did not pass")
	}

	const name = "dc-podman-integration"
	_ = rt.RemoveContainer(ctx, name, runtime.RemoveOptions{Force: true})

	if _, err := rt.PullImage(ctx, image, nil); err != nil {
		t.Fatalf("PullImage: %v", err)
	}

	// A counter that keeps its value in memory and mirrors it to a file —
	// resume vs cold restart is observable in the file.
	c, err := rt.RunContainer(ctx, runtime.RunSpec{
		Image:           image,
		Name:            name,
		Cmd:             []string{"sh", "-c", "i=0; while true; do i=$((i+1)); echo $i > /count.txt; sleep 1; done"},
		OverrideCommand: false,
	})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	t.Cleanup(func() { _ = rt.RemoveContainer(context.Background(), name, runtime.RemoveOptions{Force: true}) })

	if err := rt.StartContainer(ctx, c.ID); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	time.Sleep(6 * time.Second)

	before := readCounter(t, ctx, rt, c.ID)
	if before <= 0 {
		t.Fatalf("counter not advancing before checkpoint (got %d)", before)
	}

	arch := filepath.Join(t.TempDir(), "ckpt.tar")
	ref, err := rt.Checkpoint(ctx, c.ID, runtime.CheckpointSpec{ArchivePath: arch, StopAfter: true, TCPEstablished: true})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if ref.Size == 0 {
		t.Errorf("checkpoint archive size is 0 (%s)", ref.ArchivePath)
	}

	if err := rt.RemoveContainer(ctx, c.ID, runtime.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("RemoveContainer (source): %v", err)
	}
	time.Sleep(2 * time.Second)

	restored, err := rt.Restore(ctx, runtime.RestoreSpec{ArchivePath: arch, TCPEstablished: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = rt.RemoveContainer(context.Background(), restored.ID, runtime.RemoveOptions{Force: true}) })
	time.Sleep(3 * time.Second)

	after := readCounter(t, ctx, rt, restored.ID)
	// Resumed: the counter continues from where it was checkpointed. A
	// cold restart would be back near 1–3 (and below `before`).
	if after <= before {
		t.Fatalf("counter did not resume: before=%d after=%d (looks like a cold restart, not a restore)", before, after)
	}
	t.Logf("checkpoint/restore OK: before=%d after=%d archive=%d bytes", before, after, ref.Size)
}

func readCounter(t *testing.T, ctx context.Context, rt *Runtime, id string) int {
	t.Helper()
	res, err := rt.ExecContainer(ctx, id, runtime.ExecOptions{Cmd: []string{"cat", "/count.txt"}})
	if err != nil {
		t.Fatalf("ExecContainer(cat): %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatalf("parse counter %q: %v", res.Stdout, err)
	}
	return n
}

// TestIntegration_BuildImage builds an image with buildah via the Podman
// backend and checks the returned reference + that build logs streamed.
func TestIntegration_BuildImage(t *testing.T) {
	socket := os.Getenv("PODMAN_SOCKET")
	if socket == "" {
		t.Skip("set PODMAN_SOCKET to run the live Podman integration test")
	}
	base := os.Getenv("PODMAN_TEST_IMAGE")
	if base == "" {
		base = "docker.io/library/node:20-slim"
	}

	ctx := context.Background()
	rt, err := New(ctx, Options{Socket: socket})
	if err != nil {
		t.Fatalf("New(%q): %v", socket, err)
	}

	dir := t.TempDir()
	dockerfile := "FROM " + base + "\nRUN echo built > /built.txt\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}

	events := make(chan runtime.BuildEvent, 512)
	ref, err := rt.BuildImage(ctx, runtime.BuildSpec{ContextPath: dir, Tag: "dc-buildtest:1"}, events)
	close(events) // BuildImage has finished streaming before it returns
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if ref.ID == "" {
		t.Fatalf("BuildImage: empty image ID")
	}
	t.Cleanup(func() { _ = rt.RemoveImage(context.Background(), "dc-buildtest:1") })

	var logs int
	for e := range events {
		if e.Kind == runtime.BuildEventLog && e.Message != "" {
			logs++
		}
	}
	if logs == 0 {
		t.Errorf("expected build log events, got none")
	}
	t.Logf("buildah build OK: id=%s tags=%v logEvents=%d", ref.ID, ref.Tags, logs)
}
