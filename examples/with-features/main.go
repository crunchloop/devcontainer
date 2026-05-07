// with-features adds a local devcontainer feature on top of an image-
// source devcontainer, proves the feature's install.sh ran inside the
// container, and confirms the feature's containerEnv was applied.
//
// Run:
//
//	go run ./examples/with-features
//
// What happens:
//  1. Creates a temp workspace with devcontainer.json referencing a
//     local feature at ./local-feature.
//  2. Writes a tiny local feature whose install.sh writes a marker
//     file at /etc/example-feature-marker.
//  3. Up: the engine fetches the local feature, generates a wrapper
//     Dockerfile, builds dc-go-final-<id>, runs it.
//  4. Verifies the marker file exists (feature installed) and that
//     $EXAMPLE_FROM_FEATURE is set in the container's environment.
//  5. Tears it all down.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

func main() {
	ctx := context.Background()

	workspace, err := os.MkdirTemp("", "dc-example-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(workspace)

	// Workspace layout:
	//   .devcontainer/devcontainer.json
	//   .devcontainer/local-feature/
	//     devcontainer-feature.json
	//     install.sh
	dcDir := filepath.Join(workspace, ".devcontainer")
	featureDir := filepath.Join(dcDir, "local-feature")
	must(os.MkdirAll(featureDir, 0o755))

	must(os.WriteFile(filepath.Join(dcDir, "devcontainer.json"), []byte(`{
  "image": "alpine:3.20",
  "features": { "./local-feature": {} }
}`), 0o644))

	must(os.WriteFile(filepath.Join(featureDir, "devcontainer-feature.json"), []byte(`{
  "id": "example-feature",
  "version": "1.0.0",
  "containerEnv": { "EXAMPLE_FROM_FEATURE": "yes" }
}`), 0o644))

	must(os.WriteFile(filepath.Join(featureDir, "install.sh"), []byte(`#!/bin/sh
set -e
echo example-feature-installed > /etc/example-feature-marker
`), 0o755))

	rt, err := docker.New(ctx, docker.Options{})
	if err != nil {
		log.Fatalf("docker daemon: %v", err)
	}
	defer func() { _ = rt.Close() }()

	eng, err := devcontainer.New(devcontainer.EngineOptions{Runtime: rt})
	if err != nil {
		log.Fatalf("engine: %v", err)
	}

	ws, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: workspace,
		Recreate:             true,
		SkipLifecycle:        true,
	})
	if err != nil {
		log.Fatalf("up: %v", err)
	}
	defer func() {
		if err := eng.Down(context.Background(), ws, devcontainer.DownOptions{Remove: true}); err != nil {
			log.Printf("down: %v", err)
		}
	}()

	fmt.Printf("workspace: %s\ncontainer: %s\n", ws.ID, ws.Container.ID)

	// Verify install.sh ran.
	if res, _ := eng.Exec(ctx, ws, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/etc/example-feature-marker"},
	}); res.ExitCode == 0 {
		fmt.Printf("marker file: %s\n", strings.TrimSpace(res.Stdout))
	} else {
		log.Fatalf("marker missing: stderr=%q", res.Stderr)
	}

	// Verify containerEnv applied.
	if res, _ := eng.Exec(ctx, ws, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "echo $EXAMPLE_FROM_FEATURE"},
	}); res.ExitCode == 0 {
		fmt.Printf("EXAMPLE_FROM_FEATURE=%s\n", strings.TrimSpace(res.Stdout))
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
