//go:build integration && darwin && arm64

// Apple-container backend: image-metadata fast path. Builds an image
// carrying a devcontainer.metadata label, then exercises both the
// "metadata declares remoteUser" path and the "user override wins"
// path. Mirrors image_metadata_test.go for the docker backend.

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
	"github.com/crunchloop/devcontainer/runtime/applecontainer"
)

// buildAppleLabeledImage builds a small image carrying a
// devcontainer.metadata label + a baked-in non-root user. Uses our
// PR-G2 BuildImage path under the hood.
func buildAppleLabeledImage(t *testing.T, rt *applecontainer.Runtime, label string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir := t.TempDir()
	// Use docker.io/library/alpine:latest explicitly — Apple's image
	// resolver doesn't apply a default registry like Docker's does.
	df := `FROM docker.io/library/alpine:latest
RUN adduser -D -s /bin/sh vscode
LABEL devcontainer.metadata='` + label + `'
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}

	tag := "dc-it-ac-metadata-" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-") + ":latest"

	if _, err := rt.BuildImage(ctx, runtime.BuildSpec{
		ContextPath: dir,
		Dockerfile:  "Dockerfile",
		Tag:         tag,
	}, nil); err != nil {
		var unavail *runtime.BuilderUnavailableError
		if isUnavailErr(err, &unavail) {
			t.Skipf("builder not running (run `container builder start`): %v", err)
		}
		t.Fatalf("BuildImage: %v", err)
	}
	return tag
}

// isUnavailErr is a tiny generic-ish wrapper around errors.As so the
// per-test boilerplate stays compact. Returns true if err wraps a
// *BuilderUnavailableError.
func isUnavailErr(err error, dst **runtime.BuilderUnavailableError) bool {
	if err == nil {
		return false
	}
	// Walk the chain via Unwrap; avoid importing errors twice.
	for cur := err; cur != nil; {
		if v, ok := cur.(*runtime.BuilderUnavailableError); ok {
			*dst = v
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := cur.(unwrapper)
		if !ok {
			break
		}
		cur = u.Unwrap()
	}
	return false
}

func TestAppleContainer_ImageMetadata_RemoteUserHonored(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}

	eng, rt := newAppleContainerEngine(t)

	tag := buildAppleLabeledImage(t, rt,
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
	defer func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
	}()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"whoami"},
	})
	if err != nil {
		t.Fatalf("Exec whoami: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("whoami exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != "vscode" {
		t.Errorf("whoami = %q, want %q (image-metadata remoteUser must reach Engine.Exec)", got, "vscode")
	}
}

func TestAppleContainer_ImageMetadata_UserOverrideWins(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newAppleContainerEngine(t)

	tag := buildAppleLabeledImage(t, rt,
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
	defer func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
	}()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"whoami"},
	})
	if err != nil {
		t.Fatalf("Exec whoami: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "root" {
		t.Errorf("whoami = %q, want %q (devcontainer.json remoteUser must beat image metadata)", got, "root")
	}
}
