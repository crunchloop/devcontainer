//go:build integration && darwin && arm64

// Apple-container backend: build-source devcontainers (no features).
// Newly enabled by PR-G2's full BuildKit integration. Replaces the
// PR-H stub (TestAppleContainer_BuildSource_DocumentsLimitation) for
// the happy path; the limitation test stays as a builder-not-running
// contract assertion.

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// TestAppleContainer_BuildSource_FullLifecycle is the basic
// build-source Up/Exec/Down: devcontainer.json declares
// `build: { dockerfile: ... }`, the engine builds via PR-G2's
// BuildImage, runs the resulting image, exec confirms a baked-in
// marker file is present.
func TestAppleContainer_BuildSource_FullLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, _ := newAppleContainerEngine(t)

	ws := t.TempDir()
	dcDir := filepath.Join(ws, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dcDir, "Dockerfile"),
		"FROM docker.io/library/alpine:latest\nRUN echo built-by-bucket-a > /bucket-a-marker\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dcDir, "devcontainer.json"),
		`{"build":{"dockerfile":"Dockerfile"}}`); err != nil {
		t.Fatal(err)
	}

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

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/bucket-a-marker"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "built-by-bucket-a" {
		t.Errorf("marker contents = %q, want %q", got, "built-by-bucket-a")
	}
}

// TestAppleContainer_BuildSource_BuildArgs verifies build args
// from devcontainer.json reach the Dockerfile.
func TestAppleContainer_BuildSource_BuildArgs(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, _ := newAppleContainerEngine(t)

	ws := t.TempDir()
	dcDir := filepath.Join(ws, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dcDir, "Dockerfile"),
		"FROM docker.io/library/alpine:latest\nARG MYARG=unset\nRUN echo $MYARG > /arg-marker\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dcDir, "devcontainer.json"), `{
		"build": {
			"dockerfile": "Dockerfile",
			"args": {"MYARG": "value-from-config"}
		}
	}`); err != nil {
		t.Fatal(err)
	}

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

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/arg-marker"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "value-from-config" {
		t.Errorf("$MYARG = %q, want %q (build args not plumbed)", got, "value-from-config")
	}
}
