// image-source is the smallest end-to-end example: pull an image,
// run it, exec a command, tear it down.
//
// Run:
//
//	go run ./examples/image-source
//
// What it does:
//  1. Creates a temp workspace with a minimal devcontainer.json
//     (image: alpine:3.20).
//  2. Brings the container up (auto-pulls the image if missing).
//  3. Execs `whoami` inside the container.
//  4. Tears the container down with --rm.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

func main() {
	ctx := context.Background()

	// 1. Workspace with a minimal devcontainer.json.
	workspace, err := os.MkdirTemp("", "dc-example-*")
	if err != nil {
		log.Fatalf("tempdir: %v", err)
	}
	defer os.RemoveAll(workspace)

	dc := filepath.Join(workspace, ".devcontainer", "devcontainer.json")
	if err := os.MkdirAll(filepath.Dir(dc), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(dc, []byte(`{"image":"alpine:3.20"}`), 0o644); err != nil {
		log.Fatal(err)
	}

	// 2. Wire up Docker runtime + engine.
	rt, err := docker.New(ctx, docker.Options{})
	if err != nil {
		log.Fatalf("docker daemon: %v", err)
	}
	defer rt.Close()

	eng, err := devcontainer.New(devcontainer.EngineOptions{Runtime: rt})
	if err != nil {
		log.Fatalf("engine: %v", err)
	}

	// 3. Up.
	ws, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: workspace,
		Recreate:             true, // ensure clean container for the example
		SkipLifecycle:        true, // no lifecycle scripts in this minimal config
	})
	if err != nil {
		log.Fatalf("up: %v", err)
	}
	defer eng.Down(context.Background(), ws, devcontainer.DownOptions{Remove: true})

	fmt.Printf("workspace id: %s\ncontainer id: %s\n", ws.ID, ws.Container.ID)

	// 4. Exec.
	res, err := eng.Exec(ctx, ws, devcontainer.ExecOptions{
		Cmd: []string{"whoami"},
	})
	if err != nil {
		log.Fatalf("exec: %v", err)
	}
	fmt.Printf("whoami → %s (exit %d)\n", res.Stdout, res.ExitCode)
}
