//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/feature"
)

// TestLockfile_PublicGHCR resolves a public feature through Build (on
// Debian: the feature's install.sh needs bash, which alpine lacks),
// checks the lockfile records the registry's manifest digest for its
// tag, then shows a frozen Build fetches by the locked digest rather
// than the tag.
func TestLockfile_PublicGHCR(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newEngineWith(t, devcontainer.EngineOptions{FeatureCacheDir: t.TempDir()})
	defer rt.Close()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"image": "debian:bookworm-slim",
		"features": { "`+publicFeatureRef+`": {} }
	}`)
	lockPath := filepath.Join(dir, ".devcontainer", "devcontainer-lock.json")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	build := func(mode devcontainer.LockfileMode) error {
		_, err := eng.Build(ctx, devcontainer.BuildOptions{LocalWorkspaceFolder: dir, Lockfile: mode})
		return err
	}

	if err := build(devcontainer.LockfileDefault); err != nil {
		t.Fatalf("Build: %v", err)
	}
	lf, err := feature.ReadLockfile(lockPath)
	if err != nil || lf == nil {
		t.Fatalf("ReadLockfile: (%v, %v)", lf, err)
	}
	entry, ok := lf.Entry(publicFeatureRef)
	if !ok {
		t.Fatalf("no entry for %s: %+v", publicFeatureRef, lf)
	}

	tag, err := name.ParseReference(publicFeatureRef)
	if err != nil {
		t.Fatal(err)
	}
	head, err := remote.Head(tag, remote.WithContext(ctx))
	if err != nil {
		t.Fatalf("HEAD %s: %v", publicFeatureRef, err)
	}
	repo := tag.Context().Name()
	if entry.Integrity != head.Digest.String() {
		t.Errorf("integrity = %s, want manifest digest %s", entry.Integrity, head.Digest)
	}
	if want := repo + "@" + head.Digest.String(); entry.Resolved != want {
		t.Errorf("resolved = %s, want %s", entry.Resolved, want)
	}
	if !regexp.MustCompile(`^1\.\d+\.\d+$`).MatchString(entry.Version) {
		t.Errorf("version = %q, want the feature's full 1.x.y version", entry.Version)
	}

	if err := build(devcontainer.LockfileFrozen); err != nil {
		t.Fatalf("frozen Build with matching lockfile: %v", err)
	}

	// Lock the tag to a digest the registry does not have: a frozen
	// Build must fail fetching it, not quietly resolve the tag.
	const missing = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	entry.Integrity = missing
	entry.Resolved = repo + "@" + missing
	lf.Features[publicFeatureRef] = entry
	if err := feature.WriteLockfile(lockPath, lf); err != nil {
		t.Fatal(err)
	}
	err = build(devcontainer.LockfileFrozen)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("frozen Build with unknown locked digest: got %v, want a fetch error for %s", err, missing)
	}
}
