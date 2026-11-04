package devcontainer

import (
	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/feature"
)

// LockfileMode controls how Up and Build use the feature lockfile
// (devcontainer-lock.json beside devcontainer.json). See feature.Lockfile.
type LockfileMode string

const (
	// LockfileDefault pins features to the lockfile's entries and
	// rewrites the lockfile when what resolved differs from it,
	// creating it if missing. Matches the reference CLI's default.
	LockfileDefault LockfileMode = ""

	// LockfileFrozen pins features to the lockfile's entries and
	// returns a *feature.LockfileMismatchError, before building, when
	// the lockfile is missing or the features no longer resolve to
	// exactly its entries (a feature added, removed or re-keyed).
	// Never writes. For CI and reproducible builds.
	LockfileFrozen LockfileMode = "frozen"

	// LockfileDisabled neither reads nor writes the lockfile.
	LockfileDisabled LockfileMode = "disabled"

	// LockfileUpgrade ignores the lockfile's pins, resolves every
	// feature reference afresh, and rewrites the lockfile with the
	// result.
	LockfileUpgrade LockfileMode = "upgrade"
)

// featureLock is one Up or Build call's view of the feature lockfile.
type featureLock struct {
	mode     LockfileMode
	path     string
	existing *feature.Lockfile
}

// loadFeatureLock reads the lockfile for the devcontainer.json at
// configPath. A frozen lock fails here, before any fetch, when there
// is no lockfile.
func loadFeatureLock(mode LockfileMode, configPath string) (*featureLock, error) {
	l := &featureLock{mode: mode, path: feature.LockfilePath(configPath)}
	if mode == LockfileDisabled {
		return l, nil
	}
	existing, err := feature.ReadLockfile(l.path)
	if err != nil {
		return nil, err
	}
	if existing == nil && mode == LockfileFrozen {
		return nil, &feature.LockfileMismatchError{Path: l.path, Missing: true}
	}
	l.existing = existing
	return l, nil
}

// pin returns the entry f must resolve to, if any.
func (l *featureLock) pin(f config.ResolvedFeature) (feature.LockedFeature, bool) {
	if l.mode == LockfileUpgrade || f.SourceKind == config.FeatureSourceLocal {
		return feature.LockedFeature{}, false
	}
	return l.existing.Entry(f.Ref)
}

// acceptsBaked reports whether the base image's installation of f,
// recorded by the label entry baked, may stand in for fetching f. With
// the lockfile disabled the label match alone decides. Otherwise the
// baked artifact must be the one the lockfile pins, or, with no pin,
// one the label records, so that it can be locked. A baked feature of
// different or unknown content is fetched and installed instead: the
// label does not record the artifact when another tool built the image.
func (l *featureLock) acceptsBaked(f config.ResolvedFeature, baked config.FeatureMetadata) bool {
	if l.mode == LockfileDisabled {
		return true
	}
	if baked.ResolvedRef == "" {
		return false
	}
	pin, pinned := l.pin(f)
	return !pinned || feature.CheckIntegrity(f, feature.Fetched{ResolvedRef: baked.ResolvedRef}, pin) == nil
}

// commit records what features resolved to: written in default and
// upgrade modes when it differs from the existing lockfile, compared
// in frozen mode.
//
// A feature the base image carries keeps its pin, which acceptsBaked
// checked it against; the label does not record dependsOn, so an entry
// rebuilt from it could differ. Without a pin it is locked from the
// artifact the label records.
func (l *featureLock) commit(features []config.ResolvedFeature) error {
	if l.mode == LockfileDisabled {
		return nil
	}
	next := &feature.Lockfile{Features: map[string]feature.LockedFeature{}}
	for _, f := range features {
		if pin, pinned := l.pin(f); f.AlreadyInstalled && pinned {
			next.Features[f.Ref] = pin
			continue
		}
		if entry, ok := feature.Lock(f); ok {
			next.Features[f.Ref] = entry
		}
	}
	if next.Equal(l.existing) {
		return nil
	}
	if l.mode == LockfileFrozen {
		return &feature.LockfileMismatchError{Path: l.path, Changed: next.Changed(l.existing)}
	}
	return feature.WriteLockfile(l.path, next)
}

// commitNoFeatures reconciles the lockfile of a configuration that
// declares no features. Nothing is pinned, but entries left by removed
// features are stale: default and upgrade modes empty the lockfile and
// frozen mode reports them. Without a lockfile there is nothing to do
// in any mode, as with the reference CLI.
func commitNoFeatures(mode LockfileMode, configPath string) error {
	if mode == LockfileDisabled {
		return nil
	}
	l := &featureLock{mode: mode, path: feature.LockfilePath(configPath)}
	existing, err := feature.ReadLockfile(l.path)
	if err != nil || existing == nil {
		return err
	}
	l.existing = existing
	return l.commit(nil)
}
