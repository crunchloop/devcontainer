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
)

// TestBuildContext_SurvivesSymlinks builds a Dockerfile-source workspace
// whose context directory contains a symlink that is not referenced by
// any COPY instruction. Regression guard for a tar-packing bug where
// symlink entries were written with TypeSymlink + empty Linkname; some
// tar readers (including dockerd's older path) reject those as
// malformed and abort the build with an opaque tar error.
//
// The presence of a symlink in node_modules/.bin/* or vendored
// dependencies is the canonical real-world case — Dockerfile usually
// only COPYs ./pkg/, but the daemon still has to stream the whole
// context tar past its reader before it can prune.
func TestBuildContext_SurvivesSymlinks(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	dir := t.TempDir()

	// Real file in the context that the symlink will point at.
	mustWrite(t, filepath.Join(dir, "target.txt"), "real-content\n")

	// Relative symlink target.txt → ./target.txt, mimicking
	// node_modules/.bin/* link layout.
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Trivial Dockerfile that only COPYs the regular file. The symlink
	// is unreferenced — the test is that the tar stream survives, not
	// that COPY of the symlink works.
	mustWrite(t, filepath.Join(dir, "Dockerfile"), `
FROM alpine:3.20
COPY target.txt /etc/target.txt
RUN echo built > /etc/symlink-build-marker
`)

	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"build": { "dockerfile": "Dockerfile", "context": ".." }
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: dir,
		Recreate:             true,
		SkipLifecycle:        true,
	})
	if err != nil {
		t.Fatalf("Up with symlinked context: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/etc/symlink-build-marker"},
	})
	if err != nil {
		t.Fatalf("Exec marker: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "built") {
		t.Errorf("marker not present (build did not complete?): exit=%d stdout=%q stderr=%q",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}
