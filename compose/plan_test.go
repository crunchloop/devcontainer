package compose

import (
	"errors"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

func TestValidate_NilProject(t *testing.T) {
	p := &Plan{}
	if err := p.Validate(); err == nil {
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
	if err := p.Validate(); err != nil {
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
	err := p.Validate()
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
	err := p.Validate()
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
	if err := p.Validate(); err != nil {
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
	if err := p.Validate(); err != nil {
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
	if err := p.Validate(); err != nil {
		t.Errorf("single-service volume must be accepted: %v", err)
	}
}
