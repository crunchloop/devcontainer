package feature

import (
	"testing"

	"github.com/crunchloop/devcontainer/config"
)

func TestMatches_Permissive(t *testing.T) {
	cases := []struct {
		name            string
		bakedID, bakedV string
		reqID, reqV     string
		want            bool
	}{
		{"id mismatch", "node", "1.5.0", "git", "1.0.0", false},
		{"empty req version is any", "node", "1.5.0", "node", "", true},
		{"baked >= req: same", "node", "1.5.0", "node", "1.5.0", true},
		{"baked > req: patch", "node", "1.5.1", "node", "1.5.0", true},
		{"baked > req: minor", "node", "1.6.0", "node", "1.5.0", true},
		{"baked > req: major", "node", "2.0.0", "node", "1.5.0", true},
		{"baked < req: rejected", "node", "1.4.0", "node", "1.5.0", false},
		{"non-semver baked falls back to strict", "git", "edge", "git", "edge", true},
		{"non-semver mismatch", "git", "edge", "git", "stable", false},
		{"with v prefix", "node", "v1.5.0", "node", "v1.5.0", true},
		{"prerelease ignored", "node", "1.5.0-rc1", "node", "1.5.0", true},
		{"empty baked id never matches", "", "1.0", "x", "1.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Matches(
				config.FeatureMetadata{ID: tc.bakedID, Version: tc.bakedV},
				config.FeatureMetadata{ID: tc.reqID, Version: tc.reqV},
				MatchPermissive,
			)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatches_Strict_VersionEquality(t *testing.T) {
	if !Matches(
		config.FeatureMetadata{ID: "node", Version: "1.5.0"},
		config.FeatureMetadata{ID: "node", Version: "1.5.0"},
		MatchStrict,
	) {
		t.Error("strict mode: identical versions should match")
	}
	if Matches(
		config.FeatureMetadata{ID: "node", Version: "1.5.1"},
		config.FeatureMetadata{ID: "node", Version: "1.5.0"},
		MatchStrict,
	) {
		t.Error("strict mode: newer version should NOT satisfy")
	}
}

func TestMatchesResolved_StrictDigest(t *testing.T) {
	baked := config.ResolvedFeature{
		ResolvedRef: "ghcr.io/x/git@sha256:aaa",
		Metadata:    config.FeatureMetadata{ID: "git", Version: "1.0"},
	}
	reqSameDigest := config.ResolvedFeature{
		ResolvedRef: "ghcr.io/x/git@sha256:aaa",
		Metadata:    config.FeatureMetadata{ID: "git", Version: "1.0"},
	}
	reqDifferentDigest := config.ResolvedFeature{
		ResolvedRef: "ghcr.io/x/git@sha256:bbb",
		Metadata:    config.FeatureMetadata{ID: "git", Version: "1.0"},
	}
	if !MatchesResolved(baked, reqSameDigest, MatchStrict) {
		t.Error("strict: same digest should match")
	}
	if MatchesResolved(baked, reqDifferentDigest, MatchStrict) {
		t.Error("strict: different digest should NOT match even with same version")
	}
	if !MatchesResolved(baked, reqDifferentDigest, MatchPermissive) {
		t.Error("permissive: ID + version should match regardless of digest")
	}
}

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"1.2.3", [3]int{1, 2, 3}, true},
		{"v1.2.3", [3]int{1, 2, 3}, true},
		{"1.2", [3]int{1, 2, 0}, true},
		{"1", [3]int{1, 0, 0}, true},
		{"1.2.3-rc1", [3]int{1, 2, 3}, true},
		{"1.2.3+build", [3]int{1, 2, 3}, true},
		{"latest", [3]int{}, false},
		{"", [3]int{}, false},
		{"1.2.3.4", [3]int{}, false},
	}
	for _, tc := range cases {
		got, ok := parseSemver(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseSemver(%q) = %v %v, want %v %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
