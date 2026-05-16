//go:build integration && darwin && arm64

// End-to-end compose source on the apple-container backend. The
// load-bearing question this file answers: can compose.Orchestrator
// (PR13) drive runtime/applecontainer (PR15) through a real Up + Exec
// cycle against the apple/container apiserver running on macOS?
//
// Refuses gracefully when the daemon isn't reachable. Assumes the
// apple builder isn't running (no `container builder start`) since
// our compose fixture uses image-only services — no feature builds
// triggered.
//
// Documented constraint: apple's networking has no built-in
// service-name DNS (design probe 3). The orchestrator patches
// /etc/hosts post-level so depends_on-declared edges resolve.
// Intra-level peers without an explicit edge can still race; this
// test gates `app` on `db` via depends_on to stay inside the
// supported semantic.

package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// writeAppleComposeWorkspace builds a 2-service compose fixture
// suitable for the apple backend: alpine `app` long-sleeping +
// alpine `db` long-sleeping, with depends_on. No features (apple
// builder may not be running locally), no published ports
// (apple's vmnet on macOS 15 doesn't reliably surface them to the
// host anyway — irrelevant for the in-VM peer-resolution test).
func writeAppleComposeWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "docker-compose.yml"), `
services:
  app:
    image: docker.io/library/alpine:3.20
    command: ["sh", "-c", "while sleep 1000; do :; done"]
    depends_on:
      - db
  db:
    image: docker.io/library/alpine:3.20
    command: ["sh", "-c", "while sleep 1000; do :; done"]
`)
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"dockerComposeFile": "../docker-compose.yml",
		"service": "app",
		"workspaceFolder": "/workspaces/proj"
	}`)
	return dir
}

func TestAppleContainer_Compose_Native_FullFlow(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	// Reuse the apple-container engine constructor but layer in the
	// ComposeBackend flag. Skips if the apiserver isn't running.
	_, rt := newAppleContainerEngine(t)
	defer func() {}()
	eng, err := devcontainer.New(devcontainer.EngineOptions{
		Runtime:        rt,
		ComposeBackend: devcontainer.ComposeBackendNative,
	})
	if err != nil {
		t.Fatalf("devcontainer.New: %v", err)
	}

	ws := writeAppleComposeWorkspace(t)

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

	if wsObj.Container == nil {
		t.Fatal("Workspace.Container is nil")
	}
	if got := wsObj.Container.Labels[devcontainer.LabelDevcontainerID]; got != string(wsObj.ID) {
		t.Errorf("dev.containers.id label = %q, want %q", got, wsObj.ID)
	}
	if _, ok := wsObj.Container.Labels["com.docker.compose.project"]; !ok {
		t.Errorf("compose project label missing; container.Labels = %v", wsObj.Container.Labels)
	}

	// Diagnostics before the assertion.
	if dump, derr := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/etc/hosts"},
	}); derr == nil {
		t.Logf("/etc/hosts content:\n%s", dump.Stdout)
	}

	// /etc/hosts patch must have landed: `db` resolves from inside `app`.
	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "getent ahosts db | head -1"},
	})
	if err != nil {
		t.Fatalf("Exec lookup: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout == "" {
		t.Errorf("db not resolvable from app (hosts-patch failed?): exit=%d stdout=%q stderr=%q",
			res.ExitCode, res.Stdout, res.Stderr)
	}

	// Sentinel marker present too — defensive check that the patch
	// went through ours, not some other mechanism.
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"grep", "-q", "devcontainer-go compose hosts patch", "/etc/hosts"},
	})
	if err != nil {
		t.Fatalf("Exec grep marker: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("hosts-patch marker not found in /etc/hosts (stderr=%q)", res.Stderr)
	}

	// Workspace bind mount applied.
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd:        []string{"pwd"},
		WorkingDir: wsObj.Config.ContainerWorkspaceFolder,
	})
	if err != nil {
		t.Fatalf("Exec pwd: %v", err)
	}
	if !strings.Contains(res.Stdout, wsObj.Config.ContainerWorkspaceFolder) {
		t.Errorf("pwd = %q, want containerWorkspaceFolder = %q",
			res.Stdout, wsObj.Config.ContainerWorkspaceFolder)
	}
}
