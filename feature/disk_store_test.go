package feature

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/crunchloop/devcontainer/config"
)

func writeLocalFeature(t *testing.T, body, install string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "devcontainer-feature.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte(install), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDiskStore_FetchLocal(t *testing.T) {
	store, err := NewDiskStore(DiskStoreOptions{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	dir := writeLocalFeature(t, `{
		"id": "myfeature",
		"version": "1.0.0",
		"options": {
			"flavor": {"type":"string","default":"vanilla"}
		}
	}`, "#!/bin/sh\necho install")

	got, err := store.Fetch(context.Background(), dir, config.FeatureSourceLocal)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Dir != dir {
		t.Errorf("Dir = %q, want %q", got.Dir, dir)
	}
	if got.ResolvedRef != dir {
		t.Errorf("ResolvedRef = %q", got.ResolvedRef)
	}
	if got.Metadata.ID != "myfeature" {
		t.Errorf("Metadata.ID = %q", got.Metadata.ID)
	}
	if got.Metadata.Options["flavor"].Default != "vanilla" {
		t.Errorf("Metadata.Options = %+v", got.Metadata.Options)
	}
}

func TestDiskStore_FetchLocal_MissingInstallScript(t *testing.T) {
	store, err := NewDiskStore(DiskStoreOptions{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "devcontainer-feature.json"), []byte(`{"id":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.Fetch(context.Background(), dir, config.FeatureSourceLocal)
	if err == nil {
		t.Fatal("expected error for missing install.sh")
	}
}

func TestDiskStore_FetchLocal_MissingMetadata(t *testing.T) {
	store, _ := NewDiskStore(DiskStoreOptions{CacheDir: t.TempDir()})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := store.Fetch(context.Background(), dir, config.FeatureSourceLocal)
	if err == nil {
		t.Fatal("expected error for missing devcontainer-feature.json")
	}
}

func TestDiskStore_FetchLocal_RejectsRelativePath(t *testing.T) {
	store, _ := NewDiskStore(DiskStoreOptions{CacheDir: t.TempDir()})
	_, err := store.Fetch(context.Background(), "./somewhere", config.FeatureSourceLocal)
	if err == nil {
		t.Fatal("expected error for relative path")
	}
}

func TestDiskStore_OCIAndHTTPSAreNotImplemented(t *testing.T) {
	store, _ := NewDiskStore(DiskStoreOptions{CacheDir: t.TempDir()})
	for _, kind := range []config.FeatureSourceKind{config.FeatureSourceOCI, config.FeatureSourceHTTPS} {
		_, err := store.Fetch(context.Background(), "irrelevant", kind)
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("kind=%s: want ErrNotImplemented, got %v", kind, err)
		}
	}
}

func TestResolveLocalRef(t *testing.T) {
	cases := []struct{ configDir, ref, want string }{
		{"/proj/.devcontainer", "./feature", "/proj/.devcontainer/feature"},
		{"/proj/.devcontainer", "../shared/feature", "/proj/shared/feature"},
		{"/proj", "/abs/path", "/abs/path"},
	}
	for _, tc := range cases {
		if got := ResolveLocalRef(tc.configDir, tc.ref); got != tc.want {
			t.Errorf("ResolveLocalRef(%q, %q) = %q, want %q", tc.configDir, tc.ref, got, tc.want)
		}
	}
}

func TestNewDiskStore_DefaultCacheDir(t *testing.T) {
	store, err := NewDiskStore(DiskStoreOptions{})
	if err != nil {
		t.Fatalf("NewDiskStore: %v", err)
	}
	if !filepath.IsAbs(store.CacheDir()) {
		t.Errorf("default cache dir should be absolute, got %q", store.CacheDir())
	}
	if !contains(store.CacheDir(), "devcontainer-go") {
		t.Errorf("default cache dir should contain 'devcontainer-go', got %q", store.CacheDir())
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
