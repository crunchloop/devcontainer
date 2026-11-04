package feature

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/crunchloop/devcontainer/config"
)

// Lockfile is the spec's devcontainer-lock.json: for every OCI and
// HTTPS feature in devcontainer.json, the exact artifact it resolved to.
// Local features are not recorded (their directory is the source).
//
// Features is keyed by the feature reference exactly as written in
// devcontainer.json. The spec says keys are lowercased; the reference
// CLI writes and looks them up verbatim, and this follows the CLI so a
// lockfile is interchangeable between the two.
type Lockfile struct {
	Features map[string]LockedFeature `json:"features"`
}

// LockedFeature is one lockfile entry.
type LockedFeature struct {
	// Version is the feature's full version from its
	// devcontainer-feature.json. Informational: resolution never
	// checks it.
	Version string `json:"version"`
	// Resolved is the pinned location: repo@digest for OCI, the URL
	// for HTTPS.
	Resolved string `json:"resolved"`
	// Integrity is the artifact digest: the manifest digest for OCI,
	// the sha256 of the tarball for HTTPS.
	Integrity string `json:"integrity"`
	// DependsOn lists the feature's dependsOn keys as declared.
	DependsOn []string `json:"dependsOn,omitempty"`
}

// LockfilePath returns the lockfile beside the devcontainer.json at
// configPath: .devcontainer-lock.json for a .devcontainer.json,
// devcontainer-lock.json otherwise (the reference CLI's naming).
func LockfilePath(configPath string) string {
	base := "devcontainer-lock.json"
	if strings.HasPrefix(filepath.Base(configPath), ".") {
		base = "." + base
	}
	return filepath.Join(filepath.Dir(configPath), base)
}

// ReadLockfile reads the lockfile at path. A missing or blank file is
// no lockfile: (nil, nil). A file that is not a lockfile is an error,
// never silently replaced.
func ReadLockfile(path string) (*Lockfile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read lockfile: %w", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var lf Lockfile
	if err := json.Unmarshal(b, &lf); err != nil {
		return nil, fmt.Errorf("parse lockfile %s: %w", path, err)
	}
	return &lf, nil
}

// WriteLockfile writes lf to path in the reference CLI's format: two
// space indent, entries sorted by key, trailing newline.
func WriteLockfile(path string, lf *Lockfile) error {
	features := lf.Features
	if features == nil {
		features = map[string]LockedFeature{}
	}
	// encoding/json sorts map keys bytewise; the CLI sorts with
	// localeCompare. They agree on the lowercase registry references
	// features use. Where they don't, neither tool rewrites the other's
	// file, since both compare content rather than bytes.
	b, err := json.MarshalIndent(Lockfile{Features: features}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	return nil
}

// Equal reports whether l and o lock the same features to the same
// artifacts. Formatting and key order do not matter; a nil lockfile
// equals only another nil lockfile.
func (l *Lockfile) Equal(o *Lockfile) bool {
	if l == nil || o == nil {
		return l == o
	}
	return len(l.Changed(o)) == 0
}

// Changed returns the keys whose entries differ between l and o,
// sorted: added, removed or re-pinned features. A nil lockfile has no
// entries.
func (l *Lockfile) Changed(o *Lockfile) []string {
	var changed []string
	for k, v := range l.entries() {
		if w, ok := o.Entry(k); !ok || !reflect.DeepEqual(v, w) {
			changed = append(changed, k)
		}
	}
	for k := range o.entries() {
		if _, ok := l.Entry(k); !ok {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed
}

// Entry returns the entry for the feature keyed ref. A nil lockfile
// has no entries.
func (l *Lockfile) Entry(ref string) (LockedFeature, bool) {
	entry, ok := l.entries()[ref]
	return entry, ok
}

func (l *Lockfile) entries() map[string]LockedFeature {
	if l == nil {
		return nil
	}
	return l.Features
}

// Lock returns the lockfile entry for a fetched feature. ok is false
// for features the lockfile does not record (local ones).
func Lock(f config.ResolvedFeature) (entry LockedFeature, ok bool) {
	var resolved string
	switch f.SourceKind {
	case config.FeatureSourceOCI:
		resolved = f.ResolvedRef
	case config.FeatureSourceHTTPS:
		resolved = f.Ref
	default:
		return LockedFeature{}, false
	}
	var dependsOn []string
	for id := range f.Metadata.DependsOn {
		dependsOn = append(dependsOn, id)
	}
	sort.Strings(dependsOn)
	return LockedFeature{
		Version:   f.Metadata.Version,
		Resolved:  resolved,
		Integrity: integrity(f.SourceKind, f.ResolvedRef),
		DependsOn: dependsOn,
	}, true
}

// PinnedRef is the reference to fetch for f when the lockfile holds
// entry for it: the OCI repository at the locked digest. Other kinds
// fetch their reference as written and are checked with
// CheckIntegrity.
func PinnedRef(f config.ResolvedFeature, entry LockedFeature) (string, error) {
	if f.SourceKind != config.FeatureSourceOCI {
		return f.Ref, nil
	}
	ref, err := name.ParseReference(f.Ref)
	if err != nil {
		return "", fmt.Errorf("parse OCI ref %q: %w", f.Ref, err)
	}
	return ref.Context().Name() + "@" + entry.Integrity, nil
}

// CheckIntegrity returns a *FeatureIntegrityError when fetched is not
// the artifact entry locks f to.
func CheckIntegrity(f config.ResolvedFeature, fetched Fetched, entry LockedFeature) error {
	if got := integrity(f.SourceKind, fetched.ResolvedRef); got != entry.Integrity {
		return &FeatureIntegrityError{Ref: f.Ref, Want: entry.Integrity, Got: got}
	}
	return nil
}

// integrity extracts the artifact digest from a Fetched.ResolvedRef:
// the digest of an OCI repo@digest, or the HTTPS content digest itself.
func integrity(kind config.FeatureSourceKind, resolvedRef string) string {
	if kind == config.FeatureSourceOCI {
		if i := strings.LastIndex(resolvedRef, "@"); i >= 0 {
			return resolvedRef[i+1:]
		}
	}
	return resolvedRef
}

// FeatureIntegrityError is returned when a feature fetched under a
// lockfile pin is not the locked artifact.
type FeatureIntegrityError struct {
	Ref  string
	Want string
	Got  string
}

func (e *FeatureIntegrityError) Error() string {
	return fmt.Sprintf("feature %s: digest %s does not match lockfile integrity %s", e.Ref, e.Got, e.Want)
}

// LockfileMismatchError is returned in frozen mode when the lockfile is
// missing or does not match what the features resolve to. Nothing is
// written in that case.
type LockfileMismatchError struct {
	Path string
	// Missing is true when no lockfile exists at Path.
	Missing bool
	// Changed lists the feature keys that would be added, removed or
	// re-pinned.
	Changed []string
}

func (e *LockfileMismatchError) Error() string {
	if e.Missing {
		return fmt.Sprintf("lockfile %s does not exist", e.Path)
	}
	return fmt.Sprintf("lockfile %s does not match: %s", e.Path, strings.Join(e.Changed, ", "))
}
