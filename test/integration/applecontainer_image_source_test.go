//go:build integration && darwin && arm64

// Apple-container backend integration tests. PR-H ships the
// image-source full-lifecycle test — Up, Exec, Down — and documents
// the subset of M2/M3 fixtures we expect to pass on this backend
// today. Build / features / compose / UID-reconcile are out of scope
// (see design/runtime-applecontainer.md §8, §9, §13.8) and will
// land alongside their respective Runtime methods (PR-G2 for build).
//
// To run: `make bridge && go test -tags=integration ./test/integration/...`
// Daemon prerequisite: `brew install container && container system start`.

package integration

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/applecontainer"
)

func newAppleContainerEngine(t *testing.T) (*devcontainer.Engine, *applecontainer.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt, err := applecontainer.New(ctx, applecontainer.Options{PingTimeoutSeconds: 5})
	if err != nil {
		var unavail *runtime.DaemonUnavailableError
		if errors.As(err, &unavail) {
			t.Skipf("apple-container daemon unreachable (`container system start` required): %v", err)
		}
		t.Fatalf("applecontainer.New: %v", err)
	}
	eng, err := devcontainer.New(devcontainer.EngineOptions{Runtime: rt})
	if err != nil {
		t.Fatalf("devcontainer.New: %v", err)
	}
	return eng, rt
}

// TestAppleContainer_ImageSource_FullLifecycle proves the engine
// integration: Up an `image:` devcontainer through the apple-container
// backend, Exec, Down. Mirrors the M2 image-source test against
// runtime/docker (image_source_test.go).
func TestAppleContainer_ImageSource_FullLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}

	eng, _ := newAppleContainerEngine(t)
	ws := writeWorkspace(t, `{
		"image": "docker.io/library/alpine:latest",
		"containerEnv": {
			"CUSTOM_VAR": "hello-from-apple-container"
		}
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Logf("Up: %s", ws)
	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		downCtx, downCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer downCancel()
		if err := eng.Down(downCtx, wsObj, devcontainer.DownOptions{Remove: true}); err != nil {
			t.Errorf("Down: %v", err)
		}
	}()

	// Exec a simple command and assert stdout + the engine-injected
	// containerEnv variable both make it through.
	execCtx, execCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer execCancel()
	res, err := eng.Exec(execCtx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"/bin/sh", "-c", "echo marker; echo CV=$CUSTOM_VAR"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("Exec exit: want 0 got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "marker") {
		t.Errorf("Exec stdout missing marker; got %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "CV=hello-from-apple-container") {
		t.Errorf("Exec stdout missing containerEnv injection; got %q", res.Stdout)
	}
}

// TestAppleContainer_BuildSource_DocumentsLimitation asserts that
// build-source devcontainers fail with our typed BuilderUnavailableError
// or the "not yet implemented" error from PR-G (whichever applies).
// This is a contract test for the partial PR-G state — when PR-G2
// lands the real build path, this test should be removed or flipped
// to assert success.
func TestAppleContainer_BuildSource_DocumentsLimitation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}

	eng, _ := newAppleContainerEngine(t)
	ws := writeWorkspace(t, `{
		"build": {
			"dockerfile": "Dockerfile"
		}
	}`)

	// Drop a minimal Dockerfile so the engine doesn't bail on missing-
	// file before reaching the runtime backend.
	dockerfilePath := ws + "/.devcontainer/Dockerfile"
	if err := os.WriteFile(dockerfilePath, []byte("FROM docker.io/library/alpine:latest\nRUN true\n"), 0o644); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err == nil {
		t.Fatal("Up: want error from apple-container build path, got nil — PR-G2 may have landed, update this test")
	}
	t.Logf("expected error from build-source on apple-container: %v", err)

	// Whichever shape we get, the message should be actionable.
	var unavail *runtime.BuilderUnavailableError
	if errors.As(err, &unavail) {
		// Builder genuinely not running. Hint should be present.
		if unavail.Hint == "" {
			t.Errorf("BuilderUnavailableError.Hint is empty")
		}
		return
	}
	// Builder up but build not implemented yet.
	if !strings.Contains(err.Error(), "not yet implemented") &&
		!strings.Contains(err.Error(), "BuildImage") {
		t.Errorf("error should mention 'not yet implemented' or BuildImage; got %v", err)
	}
}
