//go:build integration

// Package integration runs end-to-end tests against a real Docker daemon.
// Build with `-tags=integration`.
package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

// testImage is a small, fast-pulling image used across the integration
// suite. We use alpine rather than mcr.microsoft.com/devcontainers/base
// because the engine layer doesn't care about devcontainer-feature
// preinstalls — we just need a real container with a shell and the
// ability to resolve env vars. Spec-faithfulness tests against real
// devcontainer images land in M3+.
const testImage = "alpine:3.20"

func writeWorkspace(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	dc := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(dc, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dc, "devcontainer.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newEngine(t *testing.T) (*devcontainer.Engine, *docker.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt, err := docker.New(ctx, docker.Options{})
	if err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}
	eng, err := devcontainer.New(devcontainer.EngineOptions{Runtime: rt})
	if err != nil {
		_ = rt.Close()
		t.Fatalf("New: %v", err)
	}
	return eng, rt
}

func TestImageSource_FullLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{
		"image": "`+testImage+`",
		"containerEnv": {
			"CUSTOM_VAR": "hello-from-config"
		}
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Logf("Up: %s", ws)
	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true, // ensure clean slate within a stale daemon cache
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		if err := eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}); err != nil {
			t.Errorf("Down (cleanup): %v", err)
		}
	}()

	if wsObj.Container.State != runtime.StateRunning {
		t.Errorf("container state = %q, want running", wsObj.Container.State)
	}
	if wsObj.Container.ID == "" {
		t.Error("container ID is empty")
	}
	if wsObj.Container.Labels[devcontainer.LabelDevcontainerID] != string(wsObj.ID) {
		t.Errorf("devcontainer id label mismatch: labels=%v id=%q", wsObj.Container.Labels, wsObj.ID)
	}

	// Exec: a literal command, asserts the container is reachable.
	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "echo ok"},
	})
	if err != nil {
		t.Fatalf("Exec echo: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exec echo exit = %d, want 0; stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "ok") {
		t.Errorf("exec echo stdout = %q, want contains 'ok'", res.Stdout)
	}

	// Exec with ${containerEnv:*} substitution. The container has CUSTOM_VAR
	// set via containerEnv in devcontainer.json; the engine should resolve
	// the placeholder against the live container's env.
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "echo ${containerEnv:CUSTOM_VAR}"},
	})
	if err != nil {
		t.Fatalf("Exec containerEnv: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exec containerEnv exit = %d", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello-from-config") {
		t.Errorf("containerEnv substitution failed: stdout=%q", res.Stdout)
	}
}

func TestImageSource_ReattachStopped(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{"image":"`+testImage+`"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	first, err := eng.Up(ctx, devcontainer.UpOptions{LocalWorkspaceFolder: ws, Recreate: true})
	if err != nil {
		t.Fatalf("first Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), first, devcontainer.DownOptions{Remove: true}) }()

	if err := eng.Down(ctx, first, devcontainer.DownOptions{}); err != nil {
		t.Fatalf("Down (no remove): %v", err)
	}

	second, err := eng.Up(ctx, devcontainer.UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if second.Container.ID != first.Container.ID {
		t.Errorf("container id changed across re-attach: first=%q second=%q",
			first.Container.ID, second.Container.ID)
	}
	if second.Container.State != runtime.StateRunning {
		t.Errorf("container state after re-attach = %q", second.Container.State)
	}
}

func TestImageSource_BuildSourceUnsupported(t *testing.T) {
	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{"build":{"dockerfile":"Dockerfile"}}`)
	_, err := eng.Up(context.Background(), devcontainer.UpOptions{LocalWorkspaceFolder: ws})
	if !errors.Is(err, runtime.ErrNotImplemented) {
		t.Errorf("expected ErrNotImplemented for build source, got %v", err)
	}
}
