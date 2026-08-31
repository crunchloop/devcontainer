package compose

import (
	"errors"
	"testing"

	"github.com/crunchloop/devcontainer/runtime"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

func dockerCaps() runtime.Capabilities {
	return runtime.Capabilities{ExitCodes: true, ServiceNameDNS: true}
}

func TestValidate_NilProject(t *testing.T) {
	p := &Plan{}
	if err := p.Validate(dockerCaps()); err == nil {
		t.Fatal("want error on nil project")
	}
}

func TestValidate_Clean(t *testing.T) {
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{Name: "app", Image: "alpine"},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	if err := p.Validate(dockerCaps()); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidate_RefusesSwarmFields(t *testing.T) {
	proj := &composetypes.Project{
		Secrets: composetypes.Secrets{
			"db-pw": composetypes.SecretConfig{Name: "db-pw"},
		},
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{
				Name:   "app",
				Image:  "alpine",
				Deploy: &composetypes.DeployConfig{Mode: "global"},
			},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	err := p.Validate(dockerCaps())
	var unsup *UnsupportedFieldError
	if !errors.As(err, &unsup) {
		t.Fatalf("want *UnsupportedFieldError, got %T: %v", err, err)
	}
	if len(unsup.Fields) != 2 {
		t.Errorf("want 2 fields, got %d: %+v", len(unsup.Fields), unsup.Fields)
	}
}

func TestValidate_RefusesScaleMulti(t *testing.T) {
	scale := 3
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{Name: "app", Image: "alpine", Scale: &scale},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	err := p.Validate(dockerCaps())
	var unsup *UnsupportedFieldError
	if !errors.As(err, &unsup) {
		t.Fatalf("want *UnsupportedFieldError, got %T: %v", err, err)
	}
}

func TestValidate_AcceptsScaleOne(t *testing.T) {
	scale := 1
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{Name: "app", Image: "alpine", Scale: &scale},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	if err := p.Validate(dockerCaps()); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidate_AcceptsServiceStarted(t *testing.T) {
	// service_started is the v1 / default condition — no health
	// gate, just "exists."
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{
				Name: "app", Image: "alpine",
				DependsOn: composetypes.DependsOnConfig{
					"db": composetypes.ServiceDependency{Condition: "service_started"},
				},
			},
			"db": composetypes.ServiceConfig{Name: "db", Image: "postgres"},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	if err := p.Validate(dockerCaps()); err != nil {
		t.Errorf("service_started must be accepted: %v", err)
	}
}

func TestValidate_AcceptsSingleServiceVolume(t *testing.T) {
	proj := &composetypes.Project{
		Volumes: composetypes.Volumes{
			"data": composetypes.VolumeConfig{Name: "data"},
		},
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{
				Name: "app", Image: "alpine",
				Volumes: []composetypes.ServiceVolumeConfig{
					{Type: composetypes.VolumeTypeVolume, Source: "data", Target: "/data"},
				},
			},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	if err := p.Validate(dockerCaps()); err != nil {
		t.Errorf("single-service volume must be accepted: %v", err)
	}
}

// TestValidate_RefusesCompletedSuccessfullyWithoutExitCodes pins the
// one condition still refused at plan time. A backend that does not
// surface exit codes reports the zero value for a stopped container,
// which is indistinguishable from a clean exit — so the gate cannot
// tell a failed job from a successful one and the plan is refused
// before any side effect.
func TestValidate_RefusesCompletedSuccessfullyWithoutExitCodes(t *testing.T) {
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{
				Name: "app", Image: "alpine",
				DependsOn: composetypes.DependsOnConfig{
					"setup": composetypes.ServiceDependency{Condition: "service_completed_successfully"},
				},
			},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}

	err := p.Validate(runtime.Capabilities{ServiceNameDNS: true})
	var bad *UnsupportedFeatureOnBackendError
	if !errors.As(err, &bad) {
		t.Fatalf("want *UnsupportedFeatureOnBackendError, got %T: %v", err, err)
	}
	if bad.Capability != "ExitCodes" {
		t.Errorf("Capability = %q, want ExitCodes", bad.Capability)
	}
	// The same plan is accepted when the backend does surface them.
	if err := p.Validate(dockerCaps()); err != nil {
		t.Errorf("want accepted on a backend with ExitCodes: %v", err)
	}
}
