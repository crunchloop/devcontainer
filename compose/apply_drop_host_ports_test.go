package compose

import (
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

func portProject() *composetypes.Project {
	return &composetypes.Project{
		Services: composetypes.Services{
			"app": {
				Name:  "app",
				Image: "app:dev",
				Ports: []composetypes.ServicePortConfig{
					{Target: 8080, Published: "8080", Protocol: "tcp"},
					{Target: 9229, Published: "9229", HostIP: "127.0.0.1", Protocol: "tcp"},
				},
				Environment: composetypes.MappingWithEquals{},
			},
			"db": {
				Name:  "db",
				Image: "postgres:17",
				Ports: []composetypes.ServicePortConfig{
					// `ports: ["5432"]` — no published side; the daemon
					// picks an ephemeral host port, in the same namespace.
					{Target: 5432},
				},
			},
			"cache": {
				Name:  "cache",
				Image: "redis:7",
			},
		},
	}
}

// Every entry goes, on every service, and the caller learns what was
// taken so it can say so. Service order is sorted: the engine turns
// each entry into a warning, and warning order must not depend on map
// iteration.
func TestApplyDropHostPorts_DropsEveryEntry(t *testing.T) {
	proj := portProject()

	dropped := ApplyDropHostPorts(proj)

	for name, svc := range proj.Services {
		if len(svc.Ports) != 0 {
			t.Errorf("service %q kept %d ports, want 0", name, len(svc.Ports))
		}
	}
	if len(dropped) != 3 {
		t.Fatalf("dropped = %d entries, want 3; got %+v", len(dropped), dropped)
	}
	want := []string{"app", "app", "db"}
	for i, svc := range want {
		if dropped[i].Service != svc {
			t.Errorf("dropped[%d].Service = %q, want %q (sorted by service name)", i, dropped[i].Service, svc)
		}
	}
	if got := dropped[0].String(); got != "8080:8080/tcp" {
		t.Errorf("dropped[0] = %q, want 8080:8080/tcp", got)
	}
	if got := dropped[1].String(); got != "127.0.0.1:9229:9229/tcp" {
		t.Errorf("dropped[1] = %q, want 127.0.0.1:9229:9229/tcp", got)
	}
	// Published unset: still dropped, and rendered without a host side
	// rather than as ":5432/tcp".
	if got := dropped[2].String(); got != "5432/tcp" {
		t.Errorf("dropped[2] = %q, want 5432/tcp", got)
	}
}

// The drop is surgical: nothing but ports: moves. A service that
// declared none is not rewritten at all.
func TestApplyDropHostPorts_LeavesTheRestOfTheServiceAlone(t *testing.T) {
	proj := portProject()

	ApplyDropHostPorts(proj)

	if got := proj.Services["app"].Image; got != "app:dev" {
		t.Errorf("app image = %q, want app:dev", got)
	}
	if got := proj.Services["cache"].Image; got != "redis:7" {
		t.Errorf("cache image = %q, want redis:7", got)
	}
	if len(proj.Services) != 3 {
		t.Errorf("services = %d, want 3", len(proj.Services))
	}
}

func TestApplyDropHostPorts_NoPortsIsANoOp(t *testing.T) {
	proj := &composetypes.Project{
		Services: composetypes.Services{"app": {Name: "app", Image: "app:dev"}},
	}
	if dropped := ApplyDropHostPorts(proj); dropped != nil {
		t.Errorf("dropped = %+v, want nil", dropped)
	}
	if dropped := ApplyDropHostPorts(nil); dropped != nil {
		t.Errorf("nil project: dropped = %+v, want nil", dropped)
	}
}

// The reason the drop happens here and not inside the orchestrator's
// portsOf: ConfigHash reads the project's ServiceConfig, so removing
// the entries is visible to the recreate check. A container created
// before the option was turned on published the ports and must be
// replaced, not reused.
func TestApplyDropHostPorts_ChangesConfigHash(t *testing.T) {
	proj := portProject()
	before := ConfigHash("sha256:x", proj.Services["app"])

	ApplyDropHostPorts(proj)

	after := ConfigHash("sha256:x", proj.Services["app"])
	if before == after {
		t.Error("ApplyDropHostPorts did not change ConfigHash; a container created with the publishes would be reused with them intact")
	}
}
