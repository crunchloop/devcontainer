//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

// buildLabeledImage builds a small image carrying a devcontainer.metadata
// label and a baked-in non-root user. The label is JSON; we pass it via
// LABEL in the Dockerfile so the resulting image's Config.Labels carries
// it verbatim. The user "vscode" is created with /bin/sh and a home dir
// so subsequent execs as that user resolve a real account.
//
// Returns the image tag.
func buildLabeledImage(t *testing.T, rt *docker.Runtime, label string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir := t.TempDir()
	df := `FROM ` + testImage + `
RUN adduser -D -s /bin/sh vscode
LABEL devcontainer.metadata='` + label + `'
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0644); err != nil {
		t.Fatal(err)
	}

	tag := "dc-it-metadata-" + strings.ReplaceAll(t.Name(), "/", "-") + ":latest"
	tag = strings.ToLower(tag)

	if _, err := rt.BuildImage(ctx, runtime.BuildSpec{
		ContextPath: dir,
		Dockerfile:  "Dockerfile",
		Tag:         tag,
	}, nil); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	return tag
}

// TestImageMetadata_RemoteUserHonored is the regression test for issue
// #20. The base image carries a devcontainer.metadata label declaring
// remoteUser=vscode and a baked vscode user; the user's devcontainer.json
// does NOT specify remoteUser. After Up, Exec(whoami) must return
// "vscode", matching @devcontainers/cli behavior.
func TestImageMetadata_RemoteUserHonored(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	tag := buildLabeledImage(t, rt,
		`[{"id":"common-utils","version":"2"},{"remoteUser":"vscode","containerUser":"vscode"}]`)

	ws := writeWorkspace(t, `{"image":"`+tag+`"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"whoami"},
	})
	if err != nil {
		t.Fatalf("Exec whoami: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("whoami exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	got := strings.TrimSpace(res.Stdout)
	if got != "vscode" {
		t.Errorf("whoami = %q, want %q (image-metadata remoteUser must reach Engine.Exec)", got, "vscode")
	}
}

// TestImageMetadata_UserOverrideWins verifies the precedence direction:
// the same image declares remoteUser=vscode in metadata, but the user's
// devcontainer.json sets remoteUser=root. The user value must win.
func TestImageMetadata_UserOverrideWins(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	tag := buildLabeledImage(t, rt,
		`[{"remoteUser":"vscode","containerUser":"vscode"}]`)

	ws := writeWorkspace(t, `{"image":"`+tag+`","remoteUser":"root"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"whoami"},
	})
	if err != nil {
		t.Fatalf("Exec whoami: %v", err)
	}
	got := strings.TrimSpace(res.Stdout)
	if got != "root" {
		t.Errorf("whoami = %q, want %q (devcontainer.json remoteUser must beat image metadata)", got, "root")
	}
}
