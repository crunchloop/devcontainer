// compose drives a 2-service devcontainer described by a Docker
// Compose project: a primary `app` service and a sidecar `db`. The
// engine starts both services, lets us exec on the primary, and
// tears the project down at the end.
//
// Requires: Docker daemon + Docker Compose v2 plugin (`docker compose
// version` must work).
//
// Run:
//
//	go run ./examples/compose
//
// What happens:
//  1. Creates a temp workspace with docker-compose.yml (app + db)
//     and a devcontainer.json pointing at it.
//  2. Up: the engine loads the compose project via compose-go,
//     generates dc-build.yaml + dc-run.yaml override files, invokes
//     `docker compose up -d`, and resolves the primary service's
//     container.
//  3. Exec on the primary prints what it sees and confirms the
//     sidecar is reachable on the project network by service name.
//  4. Down(Remove: true) tears the whole project down.
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

	workspace, err := os.MkdirTemp("", "dc-example-compose-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(workspace)

	must(os.WriteFile(filepath.Join(workspace, "docker-compose.yml"), []byte(`
services:
  app:
    image: alpine:3.20
    command: ["sh", "-c", "while sleep 1000; do :; done"]
    environment:
      USER_DECLARED: from-compose
  db:
    image: alpine:3.20
    command: ["sh", "-c", "while sleep 1000; do :; done"]
`), 0o644))

	dcDir := filepath.Join(workspace, ".devcontainer")
	must(os.MkdirAll(dcDir, 0o755))
	must(os.WriteFile(filepath.Join(dcDir, "devcontainer.json"), []byte(`{
  "dockerComposeFile": "../docker-compose.yml",
  "service": "app",
  "workspaceFolder": "/workspaces/proj"
}`), 0o644))

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
		if err := eng.Down(context.Background(), ws, devcontainer.DownOptions{
			Remove:        true,
			RemoveVolumes: true,
		}); err != nil {
			log.Printf("down: %v", err)
		}
	}()

	fmt.Printf("workspace: %s\nprimary container: %s\n", ws.ID, ws.Container.ID)
	fmt.Printf("compose project: %s\n", ws.Container.Labels["com.docker.compose.project"])

	// Show the user-declared env from the compose file flowing through.
	if res, _ := eng.Exec(ctx, ws, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "echo $USER_DECLARED"},
	}); res.ExitCode == 0 {
		fmt.Printf("USER_DECLARED=%s\n", strings.TrimSpace(res.Stdout))
	}

	// Sidecars are reachable on the compose-managed network by service
	// name. We try `getent` (typically present in alpine).
	if res, _ := eng.Exec(ctx, ws, devcontainer.ExecOptions{
		Cmd: []string{"getent", "hosts", "db"},
	}); res.ExitCode == 0 {
		fmt.Printf("db resolves to: %s\n", strings.TrimSpace(res.Stdout))
	} else {
		fmt.Println("db lookup tool unavailable; skipping (this is fine on minimal alpine)")
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
