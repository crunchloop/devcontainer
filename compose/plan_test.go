package compose

import (
	"errors"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/runtime"
)

func dockerCaps() runtime.Capabilities {
	return runtime.Capabilities{
		Healthchecks:     true,
		ExitCodes:        true,
		NamespaceSharing: true,
		RestartPolicies:  true,
		SharedVolumes:    true,
		ServiceNameDNS:   true,
	}
}

func appleCaps() runtime.Capabilities {
	return runtime.Capabilities{}
}

func TestValidate_NilProject(t *testing.T) {
	p := &Plan{}
	if err := p.Validate("docker", dockerCaps()); err == nil {
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
	if err := p.Validate("docker", dockerCaps()); err != nil {
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
				Deploy: &composetypes.DeployConfig{},
			},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	err := p.Validate("docker", dockerCaps())
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
	err := p.Validate("docker", dockerCaps())
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
	if err := p.Validate("docker", dockerCaps()); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidate_RefusesHealthyOnAppleCaps(t *testing.T) {
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{
				Name: "app", Image: "alpine",
				DependsOn: composetypes.DependsOnConfig{
					"db": composetypes.ServiceDependency{Condition: "service_healthy"},
				},
			},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	err := p.Validate("applecontainer", appleCaps())
	var bad *UnsupportedFeatureOnBackendError
	if !errors.As(err, &bad) {
		t.Fatalf("want *UnsupportedFeatureOnBackendError, got %T: %v", err, err)
	}
	if bad.Capability != "Healthchecks" {
		t.Errorf("capability = %q, want Healthchecks", bad.Capability)
	}
}

func TestValidate_RefusesCompletedSuccessfullyOnAppleCaps(t *testing.T) {
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
	err := p.Validate("applecontainer", appleCaps())
	var bad *UnsupportedFeatureOnBackendError
	if !errors.As(err, &bad) {
		t.Fatalf("want *UnsupportedFeatureOnBackendError, got %T: %v", err, err)
	}
	if bad.Capability != "ExitCodes" {
		t.Errorf("capability = %q, want ExitCodes", bad.Capability)
	}
}

func TestValidate_AcceptsServiceStartedOnAppleCaps(t *testing.T) {
	// service_started is the v1 / default condition — no health
	// gate, just "exists." Apple caps must allow it.
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
	if err := p.Validate("applecontainer", appleCaps()); err != nil {
		t.Errorf("service_started must be accepted: %v", err)
	}
}

func TestValidate_RefusesNamespaceSharingOnAppleCaps(t *testing.T) {
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app":     composetypes.ServiceConfig{Name: "app", Image: "alpine", NetworkMode: "service:primary"},
			"primary": composetypes.ServiceConfig{Name: "primary", Image: "alpine"},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	err := p.Validate("applecontainer", appleCaps())
	var bad *UnsupportedFeatureOnBackendError
	if !errors.As(err, &bad) {
		t.Fatalf("want *UnsupportedFeatureOnBackendError, got %T: %v", err, err)
	}
	if bad.Capability != "NamespaceSharing" {
		t.Errorf("capability = %q, want NamespaceSharing", bad.Capability)
	}
}

func TestValidate_RefusesSharedVolumeOnAppleCaps(t *testing.T) {
	proj := &composetypes.Project{
		Volumes: composetypes.Volumes{
			"data": composetypes.VolumeConfig{Name: "data"},
		},
		Services: composetypes.Services{
			"reader": composetypes.ServiceConfig{
				Name: "reader", Image: "alpine",
				Volumes: []composetypes.ServiceVolumeConfig{
					{Type: composetypes.VolumeTypeVolume, Source: "data", Target: "/data"},
				},
			},
			"writer": composetypes.ServiceConfig{
				Name: "writer", Image: "alpine",
				Volumes: []composetypes.ServiceVolumeConfig{
					{Type: composetypes.VolumeTypeVolume, Source: "data", Target: "/data"},
				},
			},
		},
	}
	p := &Plan{Project: proj, ProjectName: "dc-x"}
	err := p.Validate("applecontainer", appleCaps())
	var bad *VolumeSharedAcrossServicesError
	if !errors.As(err, &bad) {
		t.Fatalf("want *VolumeSharedAcrossServicesError, got %T: %v", err, err)
	}
	if bad.Volume != "data" {
		t.Errorf("volume = %q, want data", bad.Volume)
	}
	if len(bad.Services) != 2 {
		t.Errorf("want 2 services, got %v", bad.Services)
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
	if err := p.Validate("applecontainer", appleCaps()); err != nil {
		t.Errorf("single-service volume must be accepted: %v", err)
	}
}
