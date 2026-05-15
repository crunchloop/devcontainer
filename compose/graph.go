package compose

import (
	"sort"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// Level is a set of service names that can start in parallel — they
// have no edges to each other within the level, and all their
// dependencies are satisfied by previous levels. The orchestrator
// processes one level at a time, in order.
type Level []string

// TopoSort returns the project's services arranged as levels.
// Services within a level have no mutual dependencies and may be
// started in parallel; level[i+1] depends only on services in
// level[<=i]. Returns *CycleError if the depends_on graph contains
// a cycle.
//
// Sorting is deterministic: services within each level are returned
// in lexicographic order so tests and logs are stable.
//
// Edges come from depends_on (long + short form, both already
// normalized by compose-go to types.ServiceDependency entries) plus
// network_mode: service:<x>, which is treated as an implicit
// dependency edge for ordering even though compose-go does not put
// it in DependsOn.
func TopoSort(project *composetypes.Project) ([]Level, error) {
	services := project.Services

	// Build the dependency graph: each service -> set of services it
	// depends on. Track in-degree counts for Kahn's algorithm.
	deps := make(map[string]map[string]struct{}, len(services))
	for name := range services {
		deps[name] = map[string]struct{}{}
	}
	for name, svc := range services {
		for dep := range svc.DependsOn {
			if _, ok := services[dep]; !ok {
				continue
			}
			deps[name][dep] = struct{}{}
		}
		if nm := svc.NetworkMode; isServiceNetworkMode(nm) {
			peer := nm[len("service:"):]
			if _, ok := services[peer]; ok && peer != name {
				deps[name][peer] = struct{}{}
			}
		}
	}

	var levels []Level
	remaining := make(map[string]struct{}, len(services))
	for name := range services {
		remaining[name] = struct{}{}
	}

	for len(remaining) > 0 {
		var ready []string
		for name := range remaining {
			satisfied := true
			for dep := range deps[name] {
				if _, stillPending := remaining[dep]; stillPending {
					satisfied = false
					break
				}
			}
			if satisfied {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			// Everything left has an unsatisfied dep — must be a cycle.
			// Pick the lexicographically smallest service in the
			// remaining set and walk back through deps to recover one
			// concrete cycle for the error message.
			return nil, &CycleError{Cycle: findCycle(deps, remaining)}
		}
		sort.Strings(ready)
		levels = append(levels, Level(ready))
		for _, name := range ready {
			delete(remaining, name)
		}
	}

	return levels, nil
}

// findCycle returns one concrete cycle through `remaining` services
// in `deps` for inclusion in a CycleError. Picks a deterministic
// starting node (alphabetic min) and follows edges until it loops.
func findCycle(deps map[string]map[string]struct{}, remaining map[string]struct{}) []string {
	var seeds []string
	for name := range remaining {
		seeds = append(seeds, name)
	}
	sort.Strings(seeds)
	if len(seeds) == 0 {
		return nil
	}

	// DFS with a stack; the first back-edge produces the cycle slice.
	start := seeds[0]
	path := []string{start}
	indexInPath := map[string]int{start: 0}
	cur := start

	for {
		// Pick the lexicographically smallest still-remaining dep so
		// the walk is deterministic.
		var nexts []string
		for d := range deps[cur] {
			if _, stillPending := remaining[d]; stillPending {
				nexts = append(nexts, d)
			}
		}
		if len(nexts) == 0 {
			// Dead end without a cycle from this seed — fall back to
			// listing the remaining set as-is. Rare; the
			// "everything has an unsatisfied dep" precondition makes
			// this unreachable in practice.
			return append([]string(nil), seeds...)
		}
		sort.Strings(nexts)
		next := nexts[0]
		if idx, ok := indexInPath[next]; ok {
			// Back-edge — cycle is path[idx:] + next.
			cycle := append([]string(nil), path[idx:]...)
			cycle = append(cycle, next)
			return cycle
		}
		indexInPath[next] = len(path)
		path = append(path, next)
		cur = next
	}
}

// isServiceNetworkMode reports whether the value of `network_mode:`
// references another service's namespace (`service:<name>`). The
// orchestrator surfaces the dep edge here so topo-sort respects the
// ordering even though compose-go doesn't model it under DependsOn.
func isServiceNetworkMode(nm string) bool {
	const p = "service:"
	return len(nm) > len(p) && nm[:len(p)] == p
}
