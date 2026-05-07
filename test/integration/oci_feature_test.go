//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/feature"
)

// publicFeatureRef is a small, stable, publicly accessible OCI
// devcontainer feature used for integration tests. We pick git because
// it's tiny (no compiled binaries shipped), has been published for
// years (stable history), and does not require auth.
const publicFeatureRef = "ghcr.io/devcontainers/features/git:1"

func TestOCIFetch_PublicGHCR(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}

	cacheDir := t.TempDir()
	store, err := feature.NewDiskStore(feature.DiskStoreOptions{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("NewDiskStore: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	got, err := store.Fetch(ctx, publicFeatureRef, config.FeatureSourceOCI)
	if err != nil {
		t.Fatalf("Fetch %s: %v", publicFeatureRef, err)
	}

	if got.Metadata.ID != "git" {
		t.Errorf("Metadata.ID = %q, want git", got.Metadata.ID)
	}
	if got.Metadata.Version == "" {
		t.Error("Metadata.Version empty")
	}
	if got.ResolvedRef == publicFeatureRef {
		t.Error("ResolvedRef should be the pinned digest form, got the input ref")
	}
	if !contains(got.ResolvedRef, "@sha256:") {
		t.Errorf("ResolvedRef = %q, want '...@sha256:...' form", got.ResolvedRef)
	}

	// install.sh and devcontainer-feature.json should both be present.
	for _, name := range []string{"devcontainer-feature.json", "install.sh"} {
		if _, err := os.Stat(filepath.Join(got.Dir, name)); err != nil {
			t.Errorf("expected %s in extracted dir: %v", name, err)
		}
	}

	// Second fetch is a cache hit: no network required.
	t.Logf("first ResolvedRef: %s", got.ResolvedRef)
	got2, err := store.Fetch(ctx, publicFeatureRef, config.FeatureSourceOCI)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got2.Dir != got.Dir {
		t.Errorf("second fetch returned different Dir; cache miss?\nfirst:  %s\nsecond: %s", got.Dir, got2.Dir)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
