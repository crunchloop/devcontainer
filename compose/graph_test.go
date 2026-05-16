package compose

import (
	"errors"
	"reflect"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// projectWith builds a minimal compose Project from a map of
// service -> deps. Tests use it to compose specific dep shapes
// without dragging in YAML fixtures.
func projectWith(deps map[string][]string) *composetypes.Project {
	services := composetypes.Services{}
	for name, ds := range deps {
		svc := composetypes.ServiceConfig{Name: name}
		if len(ds) > 0 {
			svc.DependsOn = composetypes.DependsOnConfig{}
			for _, d := range ds {
				svc.DependsOn[d] = composetypes.ServiceDependency{Condition: "service_started"}
			}
		}
		services[name] = svc
	}
	return &composetypes.Project{Services: services}
}

func TestTopoSort_NoDeps(t *testing.T) {
	proj := projectWith(map[string][]string{
		"app":     nil,
		"sidecar": nil,
	})
	levels, err := TopoSort(proj)
	if err != nil {
		t.Fatalf("TopoSort: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("levels: want 1, got %d (%+v)", len(levels), levels)
	}
	if !reflect.DeepEqual(levels[0], Level{"app", "sidecar"}) {
		t.Errorf("level 0 = %v, want [app sidecar]", levels[0])
	}
}

func TestTopoSort_Chain(t *testing.T) {
	// app -> api -> db
	proj := projectWith(map[string][]string{
		"db":  nil,
		"api": {"db"},
		"app": {"api"},
	})
	levels, err := TopoSort(proj)
	if err != nil {
		t.Fatalf("TopoSort: %v", err)
	}
	want := []Level{{"db"}, {"api"}, {"app"}}
	if !reflect.DeepEqual(levels, want) {
		t.Errorf("levels = %v, want %v", levels, want)
	}
}

func TestTopoSort_FanOut(t *testing.T) {
	// db has two dependents at the same level
	proj := projectWith(map[string][]string{
		"db":  nil,
		"api": {"db"},
		"web": {"db"},
	})
	levels, err := TopoSort(proj)
	if err != nil {
		t.Fatalf("TopoSort: %v", err)
	}
	want := []Level{{"db"}, {"api", "web"}}
	if !reflect.DeepEqual(levels, want) {
		t.Errorf("levels = %v, want %v", levels, want)
	}
}

func TestTopoSort_DanglingDepIgnored(t *testing.T) {
	// app depends on a service that doesn't exist in the project.
	// compose-go normally rejects this during Load; if it ever
	// leaks through, our topo-sort should treat it as a no-op
	// rather than crashing.
	proj := projectWith(map[string][]string{
		"app": {"ghost"},
	})
	levels, err := TopoSort(proj)
	if err != nil {
		t.Fatalf("TopoSort: %v", err)
	}
	if !reflect.DeepEqual(levels, []Level{{"app"}}) {
		t.Errorf("levels = %v, want [[app]]", levels)
	}
}

func TestTopoSort_Cycle(t *testing.T) {
	proj := projectWith(map[string][]string{
		"a": {"b"},
		"b": {"a"},
	})
	_, err := TopoSort(proj)
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CycleError, got %T: %v", err, err)
	}
	// Cycle starts at lexicographic min and must contain both.
	if len(ce.Cycle) < 2 {
		t.Fatalf("cycle too short: %v", ce.Cycle)
	}
}

func TestTopoSort_NetworkModeServiceEdge(t *testing.T) {
	// app has no depends_on but network_mode: service:db.
	// Topo-sort must respect the implicit edge.
	services := composetypes.Services{
		"db":  composetypes.ServiceConfig{Name: "db"},
		"app": composetypes.ServiceConfig{Name: "app", NetworkMode: "service:db"},
	}
	proj := &composetypes.Project{Services: services}

	levels, err := TopoSort(proj)
	if err != nil {
		t.Fatalf("TopoSort: %v", err)
	}
	want := []Level{{"db"}, {"app"}}
	if !reflect.DeepEqual(levels, want) {
		t.Errorf("levels = %v, want %v", levels, want)
	}
}
