package feature

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestLockfilePath(t *testing.T) {
	for configPath, want := range map[string]string{
		"/w/.devcontainer/devcontainer.json":        "/w/.devcontainer/devcontainer-lock.json",
		"/w/.devcontainer.json":                     "/w/.devcontainer-lock.json",
		"/w/.devcontainer/python/devcontainer.json": "/w/.devcontainer/python/devcontainer-lock.json",
	} {
		if got := LockfilePath(configPath); got != want {
			t.Errorf("LockfilePath(%q) = %q, want %q", configPath, got, want)
		}
	}
}

func TestReadLockfile_MissingAndBlankAreNoLockfile(t *testing.T) {
	dir := t.TempDir()
	if lf, err := ReadLockfile(filepath.Join(dir, "missing.json")); lf != nil || err != nil {
		t.Errorf("missing: got (%v, %v), want (nil, nil)", lf, err)
	}
	blank := filepath.Join(dir, "blank.json")
	if err := os.WriteFile(blank, []byte(" \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lf, err := ReadLockfile(blank); lf != nil || err != nil {
		t.Errorf("blank: got (%v, %v), want (nil, nil)", lf, err)
	}
}

func TestReadLockfile_MalformedIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devcontainer-lock.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLockfile(path); err == nil {
		t.Fatal("expected a parse error")
	}
}

// TestWriteLockfile_CLIFormat pins the byte format to what the
// reference CLI writes (JSON.stringify(lockfile, null, 2) + '\n', keys
// sorted, dependsOn omitted when empty), so the two tools produce the
// same file.
func TestWriteLockfile_CLIFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devcontainer-lock.json")
	lf := &Lockfile{Features: map[string]LockedFeature{
		"ghcr.io/devcontainers/features/node:1": {
			Version:   "1.6.1",
			Resolved:  "ghcr.io/devcontainers/features/node@" + digestB,
			Integrity: digestB,
		},
		"ghcr.io/devcontainers/features/common-utils:2": {
			Version:   "2.5.2",
			Resolved:  "ghcr.io/devcontainers/features/common-utils@" + digestA,
			Integrity: digestA,
			DependsOn: []string{"ghcr.io/devcontainers/features/node"},
		},
	}}
	if err := WriteLockfile(path, lf); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "features": {
    "ghcr.io/devcontainers/features/common-utils:2": {
      "version": "2.5.2",
      "resolved": "ghcr.io/devcontainers/features/common-utils@` + digestA + `",
      "integrity": "` + digestA + `",
      "dependsOn": [
        "ghcr.io/devcontainers/features/node"
      ]
    },
    "ghcr.io/devcontainers/features/node:1": {
      "version": "1.6.1",
      "resolved": "ghcr.io/devcontainers/features/node@` + digestB + `",
      "integrity": "` + digestB + `"
    }
  }
}
`
	if string(got) != want {
		t.Errorf("written lockfile:\n%s\nwant:\n%s", got, want)
	}

	read, err := ReadLockfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Equal(lf) {
		t.Errorf("round trip changed entries: %v", read.Changed(lf))
	}
}

func TestWriteLockfile_EmptyWritesEmptyFeatures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devcontainer-lock.json")
	if err := WriteLockfile(path, &Lockfile{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if want := "{\n  \"features\": {}\n}\n"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLockfileChanged(t *testing.T) {
	entry := func(d string) LockedFeature { return LockedFeature{Version: "1.0.0", Resolved: "r@" + d, Integrity: d} }
	old := &Lockfile{Features: map[string]LockedFeature{
		"kept":    entry(digestA),
		"repined": entry(digestA),
		"removed": entry(digestA),
	}}
	next := &Lockfile{Features: map[string]LockedFeature{
		"kept":    entry(digestA),
		"repined": entry(digestB),
		"added":   entry(digestA),
	}}
	if got, want := next.Changed(old), []string{"added", "removed", "repined"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Changed = %v, want %v", got, want)
	}
	if next.Equal(old) {
		t.Error("Equal = true for differing lockfiles")
	}
	if got := next.Changed(nil); !reflect.DeepEqual(got, []string{"added", "kept", "repined"}) {
		t.Errorf("Changed(nil) = %v", got)
	}
	if next.Equal(nil) || (*Lockfile)(nil).Equal(next) {
		t.Error("a lockfile equals nil")
	}
	if !(*Lockfile)(nil).Equal(nil) {
		t.Error("nil does not equal nil")
	}
	// An existing lockfile with no entries is not the same as none: it
	// is written, not left absent.
	if (&Lockfile{}).Equal(nil) {
		t.Error("empty lockfile equals nil")
	}
}

func TestLock(t *testing.T) {
	meta := config.FeatureMetadata{
		Version:   "1.2.3",
		DependsOn: map[string]map[string]any{"ghcr.io/x/b:1": {}, "ghcr.io/x/a:1": {}},
	}
	oci := config.ResolvedFeature{
		Ref: "ghcr.io/x/f:1", SourceKind: config.FeatureSourceOCI,
		ResolvedRef: "ghcr.io/x/f@" + digestA, Metadata: meta,
	}
	got, ok := Lock(oci)
	want := LockedFeature{
		Version: "1.2.3", Resolved: "ghcr.io/x/f@" + digestA, Integrity: digestA,
		DependsOn: []string{"ghcr.io/x/a:1", "ghcr.io/x/b:1"},
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("OCI: got (%+v, %v), want %+v", got, ok, want)
	}

	https := config.ResolvedFeature{
		Ref: "https://example.com/devcontainer-feature-f.tgz", SourceKind: config.FeatureSourceHTTPS,
		ResolvedRef: digestB, Metadata: config.FeatureMetadata{Version: "0.1.0"},
	}
	got, ok = Lock(https)
	want = LockedFeature{Version: "0.1.0", Resolved: https.Ref, Integrity: digestB}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("HTTPS: got (%+v, %v), want %+v", got, ok, want)
	}

	local := config.ResolvedFeature{Ref: "./f", SourceKind: config.FeatureSourceLocal, ResolvedRef: "/w/f"}
	if _, ok := Lock(local); ok {
		t.Error("local feature locked")
	}
}

func TestPinnedRef(t *testing.T) {
	entry := LockedFeature{Integrity: digestA}
	for ref, want := range map[string]string{
		"ghcr.io/x/f:1":          "ghcr.io/x/f@" + digestA,
		"ghcr.io/x/f":            "ghcr.io/x/f@" + digestA,
		"ghcr.io/X/F:1":          "ghcr.io/x/f@" + digestA,
		"ghcr.io/x/f@" + digestB: "ghcr.io/x/f@" + digestA,
	} {
		got, err := PinnedRef(config.ResolvedFeature{Ref: ref, SourceKind: config.FeatureSourceOCI}, entry)
		if err != nil || got != want {
			t.Errorf("PinnedRef(%q) = (%q, %v), want %q", ref, got, err, want)
		}
	}
	url := "https://example.com/devcontainer-feature-f.tgz"
	if got, _ := PinnedRef(config.ResolvedFeature{Ref: url, SourceKind: config.FeatureSourceHTTPS}, entry); got != url {
		t.Errorf("HTTPS PinnedRef = %q, want the URL", got)
	}
}

func TestCheckIntegrity(t *testing.T) {
	https := config.ResolvedFeature{Ref: "https://example.com/devcontainer-feature-f.tgz", SourceKind: config.FeatureSourceHTTPS}
	entry := LockedFeature{Integrity: digestA}
	if err := CheckIntegrity(https, Fetched{ResolvedRef: digestA}, entry); err != nil {
		t.Errorf("matching digest: %v", err)
	}
	err := CheckIntegrity(https, Fetched{ResolvedRef: digestB}, entry)
	var ie *FeatureIntegrityError
	if !errors.As(err, &ie) || ie.Want != digestA || ie.Got != digestB {
		t.Fatalf("got %v, want *FeatureIntegrityError", err)
	}
	if !strings.Contains(err.Error(), https.Ref) {
		t.Errorf("error %q does not name the feature", err)
	}

	oci := config.ResolvedFeature{Ref: "ghcr.io/x/f:1", SourceKind: config.FeatureSourceOCI}
	if err := CheckIntegrity(oci, Fetched{ResolvedRef: "ghcr.io/x/f@" + digestA}, entry); err != nil {
		t.Errorf("OCI matching digest: %v", err)
	}
}
