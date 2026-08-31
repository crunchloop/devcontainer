package compose

import (
	"fmt"
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
	if project == nil {
		return nil, fmt.Errorf("compose.TopoSort: nil project")
	}
	services := project.Services

	// Build the dependency graph: each service -> set of services it
	// depends on. Track in-degree counts for Kahn's algorithm.
	deps := make(map[string]map[string]struct{}, len(services))
	for name := range services {
		deps[name] = map[string]struct{}{}
	}
	for name, svc := range services {
		for _, dep := range serviceEdges(svc) {
			if _, ok := services[dep]; ok && dep != name {
				deps[name][dep] = struct{}{}
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

// serviceRefTarget returns the service name a `service:<name>`
// namespace-mode value points at, or "" when the value is anything
// else (empty, "host", "none", "container:<id>", ...).
func serviceRefTarget(v string) string {
	const p = "service:"
	if len(v) > len(p) && v[:len(p)] == p {
		return v[len(p):]
	}
	return ""
}

// serviceEdges lists the services svc depends on: depends_on entries
// plus the implicit edges from `network_mode: service:<x>` and the
// pid/ipc equivalents — joining another service's namespace requires
// that service's container to exist first.
func serviceEdges(svc composetypes.ServiceConfig) []string {
	var out []string
	for dep := range svc.DependsOn {
		out = append(out, dep)
	}
	for _, mode := range []string{svc.NetworkMode, svc.Pid, svc.Ipc} {
		if peer := serviceRefTarget(mode); peer != "" {
			out = append(out, peer)
		}
	}
	return out
}

// ServiceClosure returns names plus the transitive closure of their
// dependencies (the same edge set TopoSort orders by). `docker
// compose up <names...>` starts the named services AND everything
// they depend on; callers restricting a Plan to a service subset use
// this to reproduce that contract. Names not present in the project
// are kept verbatim (Plan validation surfaces them); the result is
// sorted for determinism.
func ServiceClosure(project *composetypes.Project, names []string) []string {
	if project == nil {
		return append([]string(nil), names...)
	}
	seen := map[string]bool{}
	queue := append([]string(nil), names...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		svc, ok := project.Services[name]
		if !ok {
			continue
		}
		queue = append(queue, serviceEdges(svc)...)
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
