package compose

import (
	"strings"
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

func TestApplyRunOverride_AppliesSecurityMetadata(t *testing.T) {
	proj := projectForApply()
	// Service already declares one cap and a privileged:false implied by zero.
	svc := proj.Services["app"]
	svc.CapAdd = []string{"NET_ADMIN"}
	proj.Services["app"] = svc

	priv := true
	initT := true
	ov := Override{
		Service:     "app",
		Privileged:  &priv,
		Init:        &initT,
		CapAdd:      []string{"SYS_ADMIN", "NET_ADMIN"}, // NET_ADMIN dup
		SecurityOpt: []string{"seccomp=unconfined"},
	}
	if err := ApplyRunOverride(proj, "app", ov); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	got := proj.Services["app"]

	if !got.Privileged {
		t.Error("Privileged not set")
	}
	if got.Init == nil || !*got.Init {
		t.Error("Init not set")
	}
	// Union: existing NET_ADMIN preserved first, SYS_ADMIN appended, no dup.
	if want := []string{"NET_ADMIN", "SYS_ADMIN"}; !equalStrings(got.CapAdd, want) {
		t.Errorf("CapAdd = %v, want %v", got.CapAdd, want)
	}
	if want := []string{"seccomp=unconfined"}; !equalStrings(got.SecurityOpt, want) {
		t.Errorf("SecurityOpt = %v, want %v", got.SecurityOpt, want)
	}
}

func TestApplyRunOverride_AppliesEntrypointWrapper(t *testing.T) {
	proj := projectForApply()
	ov := Override{
		Service:     "app",
		Entrypoints: []string{"/usr/local/share/docker-init.sh"},
	}
	if err := ApplyRunOverride(proj, "app", ov); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	ep := []string(proj.Services["app"].Entrypoint)
	if len(ep) < 4 || ep[0] != "/bin/sh" || ep[3] != "-" {
		t.Fatalf("entrypoint wrapper not applied: %v", ep)
	}
	// Native path: single-dollar (no compose re-interpolation).
	if !strings.Contains(ep[2], `exec "$@"`) || strings.Contains(ep[2], "$$") {
		t.Errorf("native entrypoint should use single-dollar exec: %q", ep[2])
	}
	if !strings.Contains(ep[2], "docker-init.sh") {
		t.Errorf("feature entrypoint missing from wrapper: %q", ep[2])
	}
}

func TestApplyRunOverride_NoEntrypointWhenNoneDeclared(t *testing.T) {
	proj := projectForApply()
	if err := ApplyRunOverride(proj, "app", Override{Service: "app"}); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	if ep := proj.Services["app"].Entrypoint; len(ep) != 0 {
		t.Errorf("entrypoint should be untouched when no feature entrypoints; got %v", ep)
	}
}

// TestApplyRunOverride_NilPrivilegedLeavesServiceValue confirms a nil
// Override.Privileged does not downgrade a user's `privileged: true`.
func TestApplyRunOverride_NilPrivilegedLeavesServiceValue(t *testing.T) {
	proj := projectForApply()
	svc := proj.Services["app"]
	svc.Privileged = true
	proj.Services["app"] = svc

	if err := ApplyRunOverride(proj, "app", Override{Service: "app"}); err != nil {
		t.Fatalf("ApplyRunOverride: %v", err)
	}
	if !proj.Services["app"].Privileged {
		t.Error("nil Override.Privileged clobbered the service's privileged:true")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
