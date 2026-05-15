package compose

import (
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

func projectForApply() *composetypes.Project {
	return &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{
				Name:  "app",
				Image: "node:18",
				Build: &composetypes.BuildConfig{Context: "."},
				Environment: composetypes.MappingWithEquals{
					"EXISTING": strPtr("yes"),
				},
				Volumes: []composetypes.ServiceVolumeConfig{
					{Type: composetypes.VolumeTypeBind, Source: "./code", Target: "/app"},
				},
				Labels: composetypes.Labels{"user.label": "kept"},
			},
		},
	}
}

func TestApplyBuildOverride_PinsImageAndClearsBuild(t *testing.T) {
	proj := projectForApply()
	if err := ApplyBuildOverride(proj, "app", "dc-final-x:latest"); err != nil {
		t.Fatalf("ApplyBuildOverride: %v", err)
	}
	svc := proj.Services["app"]
	if svc.Image != "dc-final-x:latest" {
		t.Errorf("Image=%q, want dc-final-x:latest", svc.Image)
	}
	if svc.Build != nil {
		t.Errorf("Build = %+v, want nil", svc.Build)
	}
}

func TestApplyBuildOverride_MissingService(t *testing.T) {
	proj := projectForApply()
	if err := ApplyBuildOverride(proj, "ghost", "img"); err == nil {
		t.Fatal("want error for missing service")
	}
}

func TestApplyRunOverride_AppendsVolumeAndEnvAndLabels(t *testing.T) {
	proj := projectForApply()
	ov := Override{
		Service: "app",
		ExtraBindMounts: []BindMount{
			{Source: "/host/proj", Target: "/workspaces/proj"},
		},
		ExtraEnvironment: map[string]string{"FOO": "bar"},
		Labels:           map[string]string{"dev.containers.id": "abc"},
	}
	if err := ApplyRunOverride(proj, "app", ov); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	svc := proj.Services["app"]

	// Volumes appended, not replaced.
	if len(svc.Volumes) != 2 {
		t.Fatalf("volumes: want 2, got %d (%+v)", len(svc.Volumes), svc.Volumes)
	}
	if svc.Volumes[0].Target != "/app" || svc.Volumes[1].Target != "/workspaces/proj" {
		t.Errorf("volume order or content wrong: %+v", svc.Volumes)
	}

	// Environment merged.
	if got := svc.Environment["EXISTING"]; got == nil || *got != "yes" {
		t.Error("EXISTING env dropped")
	}
	if got := svc.Environment["FOO"]; got == nil || *got != "bar" {
		t.Error("FOO env not added")
	}

	// Labels merged.
	if svc.Labels["user.label"] != "kept" {
		t.Error("existing label dropped")
	}
	if svc.Labels["dev.containers.id"] != "abc" {
		t.Error("new label not added")
	}
}

func TestApplyRunOverride_HandlesNilMaps(t *testing.T) {
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{Name: "app", Image: "alpine"},
		},
	}
	ov := Override{
		Service:          "app",
		ExtraEnvironment: map[string]string{"X": "1"},
		Labels:           map[string]string{"Y": "2"},
	}
	if err := ApplyRunOverride(proj, "app", ov); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	svc := proj.Services["app"]
	if v := svc.Environment["X"]; v == nil || *v != "1" {
		t.Error("env not seeded from nil")
	}
	if svc.Labels["Y"] != "2" {
		t.Error("labels not seeded from nil")
	}
}

// TestApplyRunOverride_RoundTripsThroughConfigHash confirms the
// orchestrator's recreate-on-change detector sees the mutation: if
// you apply an override, the resulting hash must differ from the
// pre-override hash. Otherwise, the engine would reuse a stale
// container across feature/workspace changes.
func TestApplyRunOverride_RoundTripsThroughConfigHash(t *testing.T) {
	proj := projectForApply()
	before := ConfigHash(proj.Services["app"].Image, proj.Services["app"])

	ov := Override{
		Service: "app",
		Labels:  map[string]string{"dev.containers.id": "abc"},
	}
	if err := ApplyRunOverride(proj, "app", ov); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	after := ConfigHash(proj.Services["app"].Image, proj.Services["app"])
	if before == after {
		t.Error("ApplyRunOverride did not change ConfigHash; orchestrator reuse-check would skip recreation")
	}
}
