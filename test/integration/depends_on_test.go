//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// cliDependsOnLockfile is the reference CLI's own expected lockfile for
// these features (src/test/container-features/configs/lockfile-dependson
// in devcontainers/cli): A:2 depends on E, which is a different
// artifact from the declared E:1. The fixture's third feature, an HTTPS
// tarball, is left out with its entry: it is an uncompressed tar named
// .tgz, which the HTTPS fetcher does not extract.
const cliDependsOnLockfile = `{
  "features": {
    "ghcr.io/codspace/dependson/A:2": {
      "version": "2.0.1",
      "resolved": "ghcr.io/codspace/dependson/a@sha256:932027ef71da186210e6ceb3294c3459caaf6b548d2b547d5d26be3fc4b2264a",
      "integrity": "sha256:932027ef71da186210e6ceb3294c3459caaf6b548d2b547d5d26be3fc4b2264a",
      "dependsOn": [
        "ghcr.io/codspace/dependson/E"
      ]
    },
    "ghcr.io/codspace/dependson/E": {
      "version": "2.0.0",
      "resolved": "ghcr.io/codspace/dependson/e@sha256:9f36f159c70f8bebff57f341904b030733adb17ef12a5d58d4b3d89b2a6c7d5a",
      "integrity": "sha256:9f36f159c70f8bebff57f341904b030733adb17ef12a5d58d4b3d89b2a6c7d5a"
    },
    "ghcr.io/codspace/dependson/E:1": {
      "version": "1.0.0",
      "resolved": "ghcr.io/codspace/dependson/e@sha256:90b84127edab28ecb169cd6c6f2101ce0ea1d77589cee01951fec7f879f3a11c",
      "integrity": "sha256:90b84127edab28ecb169cd6c6f2101ce0ea1d77589cee01951fec7f879f3a11c"
    }
  }
}
`

// TestDependsOn_MatchesCLILockfile builds the reference CLI's
// lockfile-dependson fixture against its expected lockfile in frozen
// mode, which passes only if the engine pulls in and locks exactly the
// features the CLI does, then checks a default build leaves the file
// byte-for-byte as the CLI wrote it.
func TestDependsOn_MatchesCLILockfile(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newEngineWith(t, devcontainer.EngineOptions{FeatureCacheDir: t.TempDir()})
	defer rt.Close()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"image": "debian:bookworm-slim",
		"features": {
			"ghcr.io/codspace/dependson/A:2": {},
			"ghcr.io/codspace/dependson/E:1": {}
		}
	}`)
	lockPath := filepath.Join(dir, ".devcontainer", "devcontainer-lock.json")
	mustWrite(t, lockPath, cliDependsOnLockfile)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, mode := range []devcontainer.LockfileMode{devcontainer.LockfileFrozen, devcontainer.LockfileDefault} {
		if _, err := eng.Build(ctx, devcontainer.BuildOptions{LocalWorkspaceFolder: dir, Lockfile: mode}); err != nil {
			t.Fatalf("Build (lockfile mode %q): %v", mode, err)
		}
	}
	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != cliDependsOnLockfile {
		t.Errorf("lockfile rewritten:\n%s", got)
	}
}
