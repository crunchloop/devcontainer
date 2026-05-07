package feature

import (
	"strconv"
	"strings"

	"github.com/crunchloop/devcontainer/config"
)

// MatchMode controls how Matches compares a baked feature entry to a
// requested feature.
type MatchMode int

const (
	// MatchPermissive (default) considers a feature "already installed"
	// if the baked id matches and the baked semver is >= the request.
	// Non-semver versions fall back to strict-string-equality —
	// guessing version ordering on arbitrary tags is unsafe.
	MatchPermissive MatchMode = iota

	// MatchStrict requires byte-level equality on the resolved digest.
	// Only set when reproducible builds matter; rebuilds are required
	// whenever any feature artifact changes upstream.
	MatchStrict
)

// Matches reports whether `baked` (an entry from a base image's
// devcontainer.metadata label) satisfies the request `req`. Used by
// Engine.Up to mark cfg.Features entries AlreadyInstalled and skip
// fetch + dockerfile-gen for them.
func Matches(baked, req config.FeatureMetadata, mode MatchMode) bool {
	if baked.ID == "" || baked.ID != req.ID {
		return false
	}
	if mode == MatchStrict {
		// Strict: digest equality. ResolvedRef lives on ResolvedFeature,
		// not Metadata, so callers route the digest comparison via the
		// MatchesResolved variant below.
		return baked.Version == req.Version
	}
	// Permissive: semver-aware "baked is at least req". Non-semver
	// versions degrade to strict string equality.
	if req.Version == "" {
		// Caller expressed no version constraint — any baked version
		// is acceptable.
		return true
	}
	bakedSemver, bakedOk := parseSemver(baked.Version)
	reqSemver, reqOk := parseSemver(req.Version)
	if !bakedOk || !reqOk {
		return baked.Version == req.Version
	}
	return semverGTE(bakedSemver, reqSemver)
}

// MatchesResolved is the strict-mode-aware version that knows about
// ResolvedRef (the digest pinning) for byte-level identity comparison.
// In permissive mode it falls through to Matches on Metadata.
func MatchesResolved(baked, req config.ResolvedFeature, mode MatchMode) bool {
	if mode == MatchStrict {
		return baked.Metadata.ID != "" &&
			baked.Metadata.ID == req.Metadata.ID &&
			baked.ResolvedRef != "" &&
			baked.ResolvedRef == req.ResolvedRef
	}
	return Matches(baked.Metadata, req.Metadata, mode)
}

// parseSemver parses "X.Y.Z" or shorter forms ("1", "1.2") into a
// 3-element [major, minor, patch] slice. Trailing pre-release / build
// metadata (after `-` or `+`) is dropped: rough but adequate for the
// feature ecosystem where version strings rarely have qualifiers.
func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimPrefix(v, "v")
	// Strip pre-release / build suffix.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func semverGTE(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}
