package feature

import (
	"github.com/crunchloop/devcontainer/config"
)

// Order returns the install order for a slice of features. Per spec:
//
//   1. Features whose id matches an entry in overrideOrder lead the
//      result, in declaration order. (Hard override; not topo-sorted.)
//   2. The rest are topo-sorted by their installsAfter and dependsOn
//      edges, with alphabetical tie-breaking.
//
// Order is idempotent: calling it twice produces the same slice.
//
// Order tolerates partially-fetched features: if Metadata is empty
// (no installsAfter / dependsOn declared), the topo sort degenerates
// to alphabetical-by-ref. This lets config.Resolve apply
// overrideFeatureInstallOrder before fetch, and the engine re-run
// Order with full metadata after fetch.
//
// Errors:
//   - *FeatureCycleError if installsAfter / dependsOn form a cycle.
//   - *FeatureDAGTooDeepError if depth exceeds the hard limit (64).
//
// Warnings: WarnDeepFeatureChain when depth exceeds the soft limit (16).
func Order(features []config.ResolvedFeature, overrideOrder []string) ([]config.ResolvedFeature, []config.Warning, error) {
	if len(features) == 0 {
		return features, nil, nil
	}

	overridden, remaining := splitByOverride(features, overrideOrder)

	sorted, warns, err := topoSort(remaining)
	if err != nil {
		return nil, warns, err
	}

	out := make([]config.ResolvedFeature, 0, len(features))
	out = append(out, overridden...)
	out = append(out, sorted...)
	return out, warns, nil
}

// splitByOverride partitions features into two groups: ones matching
// (in declaration order) the overrideOrder list, and ones not. Features
// matching multiple override entries match the first.
func splitByOverride(features []config.ResolvedFeature, overrideOrder []string) (overridden, remaining []config.ResolvedFeature) {
	if len(overrideOrder) == 0 {
		return nil, features
	}
	taken := make(map[int]bool, len(features))
	for _, ord := range overrideOrder {
		ordID := normalizeID(ord)
		for i, f := range features {
			if !taken[i] && normalizeID(f.Ref) == ordID {
				overridden = append(overridden, f)
				taken[i] = true
			}
		}
	}
	for i, f := range features {
		if !taken[i] {
			remaining = append(remaining, f)
		}
	}
	return overridden, remaining
}
