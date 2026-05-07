package feature

import (
	"archive/tar"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
)

func TestFetchHTTPS_HappyPath(t *testing.T) {
	body := makeTarball(t, []tarEntry{
		{name: "devcontainer-feature.json", mode: 0o644, body: `{"id":"my-feature","version":"1.0.0"}`},
		{name: "install.sh", mode: 0o755, body: "#!/bin/sh\n"},
	}, true)

	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if !strings.HasSuffix(r.URL.Path, "/devcontainer-feature-myfeature.tgz") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	store, _ := NewDiskStore(DiskStoreOptions{
		CacheDir:     t.TempDir(),
		HTTPSHeaders: map[string]string{"Authorization": "Bearer t0ken"},
		HTTPSClient:  srv.Client(),
	})

	ref := srv.URL + "/devcontainer-feature-myfeature.tgz"
	got, err := store.Fetch(context.Background(), ref, config.FeatureSourceHTTPS)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Metadata.ID != "my-feature" {
		t.Errorf("Metadata.ID = %q", got.Metadata.ID)
	}
	if !strings.HasPrefix(got.ResolvedRef, "sha256:") {
		t.Errorf("ResolvedRef = %q, want sha256:...", got.ResolvedRef)
	}
	if gotAuth != "Bearer t0ken" {
		t.Errorf("server saw Auth = %q, want 'Bearer t0ken'", gotAuth)
	}

	// Second fetch with the same content reuses the extracted directory.
	// (We re-download because the cache key is the body hash, not the URL —
	// so a fresh GET is needed to compute the key. URL→digest indexing
	// to skip the re-download is a future cache enhancement.)
	got2, err := store.Fetch(context.Background(), ref, config.FeatureSourceHTTPS)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got2.Dir != got.Dir {
		t.Errorf("second fetch should reuse extracted dir; got %q vs %q", got2.Dir, got.Dir)
	}
	if got2.ResolvedRef != got.ResolvedRef {
		t.Errorf("second fetch ResolvedRef differs: %q vs %q", got2.ResolvedRef, got.ResolvedRef)
	}
}

func TestFetchHTTPS_RejectsBadFilename(t *testing.T) {
	store, _ := NewDiskStore(DiskStoreOptions{CacheDir: t.TempDir()})
	_, err := store.Fetch(context.Background(), "https://example.com/feature.tgz", config.FeatureSourceHTTPS)
	if err == nil || !strings.Contains(err.Error(), "filename") {
		t.Errorf("expected filename error, got %v", err)
	}
}

func TestFetchHTTPS_RejectsNonHTTPScheme(t *testing.T) {
	store, _ := NewDiskStore(DiskStoreOptions{CacheDir: t.TempDir()})
	_, err := store.Fetch(context.Background(), "ftp://example.com/devcontainer-feature-x.tgz", config.FeatureSourceHTTPS)
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

func TestFetchHTTPS_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	store, _ := NewDiskStore(DiskStoreOptions{
		CacheDir:    t.TempDir(),
		HTTPSClient: srv.Client(),
	})

	_, err := store.Fetch(context.Background(), srv.URL+"/devcontainer-feature-x.tgz", config.FeatureSourceHTTPS)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 error, got %v", err)
	}
}

// Use tar.TypeReg to avoid the unused-import linter complaint.
var _ = tar.TypeReg
