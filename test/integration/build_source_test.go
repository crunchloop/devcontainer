//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// writeBuildSourceWorkspace creates a workspace with a Dockerfile-source
// devcontainer.json plus an inline local feature. Returns the workspace
// path. The feature, when installed, creates /etc/feature-marker so the
// test can assert it ran.
func writeBuildSourceWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// User-provided Dockerfile.
	mustWrite(t, filepath.Join(dir, "Dockerfile"), `
FROM alpine:3.20
RUN echo base-image > /etc/build-source-marker
`)

	// devcontainer.json points to a Dockerfile in the context root, with
	// context = ".." so it resolves to the workspace dir (which contains
	// Dockerfile). The Dockerfile path itself is relative to that context.
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"build": { "dockerfile": "Dockerfile", "context": ".." },
		"features": {
			"./local-feature": {}
		}
	}`)

	// Local feature: writes /etc/feature-marker on install.
	featureDir := filepath.Join(dir, ".devcontainer", "local-feature")
	mustWrite(t, filepath.Join(featureDir, "devcontainer-feature.json"), `{
		"id": "test-feature",
		"version": "1.0.0",
		"containerEnv": { "FEATURE_INSTALLED": "yes" }
	}`)
	mustWrite(t, filepath.Join(featureDir, "install.sh"), `#!/bin/sh
set -e
echo feature-ran > /etc/feature-marker
`)
	if err := os.Chmod(filepath.Join(featureDir, "install.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSource_WithLocalFeature(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeBuildSourceWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
		// Disable lifecycle so the test only validates build + features.
		SkipLifecycle: true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	// Base image's Dockerfile RUN created /etc/build-source-marker.
	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/etc/build-source-marker"},
	})
	if err != nil {
		t.Fatalf("Exec build-source-marker: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("build-source-marker missing: stderr=%q", res.Stderr)
	}
	if !strings.Contains(res.Stdout, "base-image") {
		t.Errorf("build-source-marker contents = %q", res.Stdout)
	}

	// Feature install script created /etc/feature-marker.
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/etc/feature-marker"},
	})
	if err != nil {
		t.Fatalf("Exec feature-marker: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("feature-marker missing: stderr=%q (build did not install feature?)", res.Stderr)
	}
	if !strings.Contains(res.Stdout, "feature-ran") {
		t.Errorf("feature-marker contents = %q", res.Stdout)
	}

	// Feature's containerEnv was applied.
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "echo $FEATURE_INSTALLED"},
	})
	if err != nil {
		t.Fatalf("Exec FEATURE_INSTALLED: %v", err)
	}
	if !strings.Contains(res.Stdout, "yes") {
		t.Errorf("FEATURE_INSTALLED not applied: stdout=%q", res.Stdout)
	}
}

func TestImageSource_WithLocalFeature(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), fmt.Sprintf(`{
		"image": "%s",
		"features": { "./local-feature": {} }
	}`, testImage))

	featureDir := filepath.Join(dir, ".devcontainer", "local-feature")
	mustWrite(t, filepath.Join(featureDir, "devcontainer-feature.json"), `{
		"id": "stamp",
		"version": "1.0.0",
		"containerEnv": { "STAMP": "from-feature" }
	}`)
	mustWrite(t, filepath.Join(featureDir, "install.sh"), `#!/bin/sh
set -e
echo image-source-feature-ran > /etc/feature-marker
`)
	if err := os.Chmod(filepath.Join(featureDir, "install.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: dir,
		Recreate:             true,
		SkipLifecycle:        true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/etc/feature-marker"},
	})
	if err != nil {
		t.Fatalf("Exec feature-marker: %v", err)
	}
	if !strings.Contains(res.Stdout, "image-source-feature-ran") {
		t.Errorf("feature did not run on image-source path: %q", res.Stdout)
	}
}
