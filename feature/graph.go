package feature

import (
	"fmt"
	"sort"
	"strings"

	"github.com/crunchloop/devcontainer/config"
)

// Graph depth thresholds per design/features.md §10.4.
const (
	dagWarnDepth  = 16
	dagErrorDepth = 64
)

// FeatureCycleError is returned by Order when the feature graph has a
// cycle. Path is the cycle in encounter order, repeating the start
// node at the end so it reads as a closed loop.
type FeatureCycleError struct {
	Path []string
}

func (e *FeatureCycleError) Error() string {
	return "feature graph cycle: " + strings.Join(e.Path, " -> ")
}

// FeatureDAGTooDeepError is returned by Order when the feature graph
// exceeds the hard depth limit (64).
type FeatureDAGTooDeepError struct {
	Depth int
	Path  []string // chain from a root to the deepest node
}

func (e *FeatureDAGTooDeepError) Error() string {
	return fmt.Sprintf("feature graph too deep (depth=%d > %d): %s",
		e.Depth, dagErrorDepth, strings.Join(e.Path, " -> "))
}

// topoSort orders features by their installsAfter / dependsOn edges,
// using Kahn's algorithm. Returns the ordered slice plus warnings for
// deep chains. A *FeatureCycleError is returned if any cycle is
// detected; *FeatureDAGTooDeepError if depth exceeds 64.
//
// Edge semantics: if A's metadata declares installsAfter=[B], then B
// must be installed before A — i.e., B → A in the topo graph (B has no
// incoming edges from A; A depends on B).
//
// Determinism: when multiple features have zero in-degree at the same
// step, they are taken in alphabetical order by ref.
func topoSort(features []config.ResolvedFeature) ([]config.ResolvedFeature, []config.Warning, error) {
	if len(features) <= 1 {
		return features, nil, nil
	}

	// Build a map from a feature's id (ignoring tag) to its index, so
	// installsAfter / dependsOn entries can be resolved against the
	// feature set even if their refs differ in tag specificity.
	idx := make(map[string]int, len(features))
	for i, f := range features {
		idx[normalizeID(f.Ref)] = i
	}

	// inDegree[i] = number of edges pointing TO features[i] (i.e.
	// number of dependencies still unsatisfied).
	inDegree := make([]int, len(features))
	// adj[i] = features that depend on i (out-edges).
	adj := make([][]int, len(features))

	for i, f := range features {
		for _, dep := range f.Metadata.InstallsAfter {
			j, ok := idx[normalizeID(dep)]
			if !ok {
				continue // unrelated installsAfter — ignored per spec
			}
			adj[j] = append(adj[j], i)
			inDegree[i]++
		}
		for dep := range f.Metadata.DependsOn {
			j, ok := idx[normalizeID(dep)]
			if !ok {
				continue
			}
			adj[j] = append(adj[j], i)
			inDegree[i]++
		}
	}

	// Kahn's algorithm with depth tracking. Each feature is assigned
	// the depth of its deepest predecessor + 1; the maximum depth
	// observed is the longest dependency chain.
	depth := make([]int, len(features))
	ready := make([]int, 0, len(features))
	for i, d := range inDegree {
		if d == 0 {
			ready = append(ready, i)
		}
	}
	sort.Slice(ready, func(a, b int) bool {
		return features[ready[a]].Ref < features[ready[b]].Ref
	})

	out := make([]config.ResolvedFeature, 0, len(features))
	maxDepth := 0
	deepestPath := []string{}

	for len(ready) > 0 {
		// Take the first ready feature (alphabetical because of the sort).
		i := ready[0]
		ready = ready[1:]
		out = append(out, features[i])
		if depth[i] > maxDepth {
			maxDepth = depth[i]
			deepestPath = append(deepestPath[:0], buildDeepestPath(i, depth, adj, features)...)
		}

		// Sort children alphabetically before processing for determinism.
		children := append([]int(nil), adj[i]...)
		sort.Slice(children, func(a, b int) bool {
			return features[children[a]].Ref < features[children[b]].Ref
		})
		for _, j := range children {
			if depth[i]+1 > depth[j] {
				depth[j] = depth[i] + 1
			}
			inDegree[j]--
			if inDegree[j] == 0 {
				ready = append(ready, j)
			}
		}
	}

	if len(out) < len(features) {
		// Some features still have unsatisfied in-edges → cycle.
		return nil, nil, &FeatureCycleError{Path: cyclePath(features, inDegree, adj)}
	}

	if maxDepth >= dagErrorDepth {
		return nil, nil, &FeatureDAGTooDeepError{Depth: maxDepth, Path: deepestPath}
	}
	var warnings []config.Warning
	if maxDepth >= dagWarnDepth {
		warnings = append(warnings, config.Warning{
			Code: config.WarnDeepFeatureChain,
			Message: fmt.Sprintf("feature dependency chain depth %d exceeds soft limit %d: %s",
				maxDepth, dagWarnDepth, strings.Join(deepestPath, " -> ")),
		})
	}

	return out, warnings, nil
}

// buildDeepestPath reconstructs a chain to a node at `depth[target]`
// by walking backward through predecessors. Used for diagnostics, not
// hot-path; O(N*chain_len).
func buildDeepestPath(target int, depth []int, adj [][]int, features []config.ResolvedFeature) []string {
	preds := make([][]int, len(features))
	for i, children := range adj {
		for _, j := range children {
			preds[j] = append(preds[j], i)
		}
	}
	path := []string{features[target].Ref}
	for cur := target; depth[cur] > 0; {
		var prev int = -1
		for _, p := range preds[cur] {
			if depth[p] == depth[cur]-1 {
				prev = p
				break
			}
		}
		if prev < 0 {
			break
		}
		path = append([]string{features[prev].Ref}, path...)
		cur = prev
	}
	return path
}

// cyclePath finds and returns one cycle from the remaining (in-degree
// > 0) nodes via DFS.
func cyclePath(features []config.ResolvedFeature, inDegree []int, adj [][]int) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make([]int, len(features))
	parent := make([]int, len(features))
	for i := range parent {
		parent[i] = -1
	}
	var cycleStart, cycleEnd = -1, -1

	var dfs func(u int) bool
	dfs = func(u int) bool {
		color[u] = gray
		for _, v := range adj[u] {
			if color[v] == white {
				parent[v] = u
				if dfs(v) {
					return true
				}
			} else if color[v] == gray {
				cycleStart, cycleEnd = v, u
				return true
			}
		}
		color[u] = black
		return false
	}

	for i, d := range inDegree {
		if d > 0 && color[i] == white {
			if dfs(i) {
				break
			}
		}
	}

	if cycleStart < 0 {
		// Shouldn't happen — caller guarantees a cycle exists.
		return []string{"<unknown cycle>"}
	}
	path := []string{features[cycleStart].Ref}
	for v := cycleEnd; v != cycleStart; v = parent[v] {
		path = append([]string{features[v].Ref}, path...)
	}
	path = append(path, features[cycleStart].Ref)
	return path
}

// normalizeID strips tag and digest from a feature ref so that
// installsAfter / dependsOn references match regardless of pinning.
//
//	ghcr.io/foo/bar:1.2 → ghcr.io/foo/bar
//	ghcr.io/foo/bar@sha256:abc → ghcr.io/foo/bar
func normalizeID(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		// Don't strip the scheme of an HTTPS ref.
		if !strings.Contains(ref[:i], "://") || strings.LastIndex(ref, "/") > i {
			ref = ref[:i]
		}
	}
	return ref
}
