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

// commit records what features resolved to: written in default and
// upgrade modes when it differs from the existing lockfile, compared
// in frozen mode.
//
// Features the base image already carries were not fetched, so there
// is nothing new to record for them: their existing entry is kept.
func (l *featureLock) commit(features []config.ResolvedFeature) error {
	if l.mode == LockfileDisabled {
		return nil
	}
	next := &feature.Lockfile{Features: map[string]feature.LockedFeature{}}
	for _, f := range features {
		if f.AlreadyInstalled {
			if entry, ok := l.existing.Entry(f.Ref); ok {
				next.Features[f.Ref] = entry
			}
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
