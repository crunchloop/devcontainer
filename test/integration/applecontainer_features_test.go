//go:build integration && darwin && arm64

// Apple-container backend: features probe (A2). The feature pipeline
// is engine-level Go code (feature/* + engine.up's layerFeatures path)
// that ultimately calls runtime.BuildImage — which apple-container
// now has since PR-G2. The pipeline should "just work" on this
// backend.
//
// This file ships ONE probe test. If it passes, features work on
// apple-container with no further changes. If it fails, it fails the
// suite loudly — feature support is a design-level contract on this
// backend, so a regression here is a real bug, not a "known TODO".

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

// TestAppleContainer_Features_LocalFeatureInstalls probes the feature
// pipeline end-to-end: image-source + a local feature that drops a
// marker file. Mirrors TestImageSource_WithLocalFeature in the docker
// suite.
func TestAppleContainer_Features_LocalFeatureInstalls(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, _ := newAppleContainerEngine(t)

	dir := t.TempDir()
	dcDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dcDir, "devcontainer.json"), fmt.Sprintf(`{
		"image": "%s",
		"features": { "./local-feature": {} }
	}`, "docker.io/library/alpine:latest")); err != nil {
		t.Fatal(err)
	}

	featureDir := filepath.Join(dcDir, "local-feature")
	if err := os.MkdirAll(featureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(featureDir, "devcontainer-feature.json"), `{
		"id": "stamp",
		"version": "1.0.0",
		"containerEnv": { "STAMP": "from-apple-feature" }
	}`); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(featureDir, "install.sh"), `#!/bin/sh
set -e
echo apple-feature-ran > /etc/feature-marker
`); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("feature install failed on apple-container: %v", err)
	}
	defer func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
	}()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/etc/feature-marker"},
	})
	if err != nil {
		t.Fatalf("Exec feature-marker: %v", err)
	}
	if !strings.Contains(res.Stdout, "apple-feature-ran") {
		t.Errorf("feature did not run on apple-container image-source path: %q", res.Stdout)
	}

	// STAMP env from the feature should reach Exec.
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"printenv", "STAMP"},
	})
	if err != nil {
		t.Fatalf("Exec printenv STAMP: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "from-apple-feature" {
		t.Errorf("STAMP = %q, want %q (feature containerEnv didn't reach Exec)", got, "from-apple-feature")
	}
}
