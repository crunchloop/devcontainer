//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// writeComposePortsWorkspace lays out a project whose primary service
// publishes a host port and whose sidecar answers on the compose
// network. The two together are what DisableHostPortPublishing
// claims: the publish is removable, the service-name path is not.
func writeComposePortsWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "docker-compose.yml"), `
services:
  app:
    image: `+testImage+`
    command: ["sh", "-c", "while sleep 1000; do :; done"]
    ports:
      - "39080:8080"
  db:
    image: `+testImage+`
    command: ["sh", "-c", "while true; do echo db-reachable | nc -l -p 5000; done"]
`)

	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"dockerComposeFile": "../docker-compose.yml",
		"service": "app",
		"runServices": ["app", "db"],
		"workspaceFolder": "/workspaces/proj"
	}`)
	return dir
}

// The option's claim is that dropping the host side of `ports:` is
// semantically safe. This is the half a fake runtime cannot make: the
// project still boots against a real daemon, and the sidecar is still
// reachable by service name over the compose network — which is how
// an embedder reaches it, and why nothing is lost by not publishing.
//
// What the host binding itself does (or, here, does not do) is
// asserted at the RunSpec boundary in the unit tests; the daemon-side
// translation of RunSpec.Ports is covered in runtime/docker.
func TestComposeSource_DisableHostPortPublishing_BootsAndKeepsServiceDNS(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngineWith(t, devcontainer.EngineOptions{
		ComposeBackend:            devcontainer.ComposeBackendNative,
		DisableHostPortPublishing: true,
	})
	defer rt.Close()

	ws := writeComposePortsWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
		SkipLifecycle:        true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{
			Remove:        true,
			RemoveVolumes: true,
		})
	}()

	// Retry: nothing orders the sidecar's listener against this exec,
	// and busybox nc serves one connection per accept.
	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", `for i in $(seq 1 30); do out=$(nc -w 5 db 5000); if [ -n "$out" ]; then echo "$out"; exit 0; fi; sleep 1; done; exit 1`},
	})
	if err != nil {
		t.Fatalf("Exec nc db: %v", err)
	}
	if !strings.Contains(res.Stdout, "db-reachable") {
		t.Errorf("sidecar unreachable by service name: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}
