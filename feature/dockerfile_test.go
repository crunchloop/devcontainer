package feature

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
)

// fixtureFeature creates a small on-disk feature with the given id and
// returns its directory. Used to feed BuildPlan.Features.
func fixtureFeature(t *testing.T, id, version string) config.ResolvedFeature {
	t.Helper()
	dir := t.TempDir()
	body := `{"id":"` + id + `","version":"` + version + `","containerEnv":{"VARS_FROM_` + id + `":"hello"}}`
	if err := os.WriteFile(filepath.Join(dir, "devcontainer-feature.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte("#!/bin/sh\nset -e\n# install "+id+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return config.ResolvedFeature{
		Ref:         "ghcr.io/test/" + id + ":" + version,
		ResolvedRef: "ghcr.io/test/" + id + "@sha256:deadbeef",
		Dir:         dir,
		SourceKind:  config.FeatureSourceOCI,
		Options:     map[string]any{"version": version},
		Metadata: config.FeatureMetadata{
			ID:      id,
			Version: version,
			ContainerEnv: map[string]string{
				"VARS_FROM_" + id: "hello",
			},
		},
	}
}

func TestGenerateBuildContext_WritesExpectedLayout(t *testing.T) {
	dst := t.TempDir()
	plan := BuildPlan{
		BaseImage:  "alpine:3.20",
		RemoteUser: "vscode",
		Features: []config.ResolvedFeature{
			fixtureFeature(t, "git", "1.0"),
			fixtureFeature(t, "node", "20"),
		},
	}
	if err := GenerateBuildContext(plan, dst); err != nil {
		t.Fatalf("GenerateBuildContext: %v", err)
	}

	// Dockerfile present.
	df, err := os.ReadFile(filepath.Join(dst, "Dockerfile"))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}

	wantSubstrings := []string{
		"# syntax=docker/dockerfile:1.4",
		"ARG _DEV_CONTAINERS_BASE_IMAGE=alpine:3.20",
		"FROM $_DEV_CONTAINERS_BASE_IMAGE",
		"COPY ./build-context/ /tmp/dc-features/",
		"# Feature 0: ghcr.io/test/git:1.0",
		`ENV VARS_FROM_git="hello"`,
		"RUN cd /tmp/dc-features/0",
		"# Feature 1: ghcr.io/test/node:20",
		"RUN cd /tmp/dc-features/1",
		"LABEL devcontainer.metadata=",
		"ARG _DEV_CONTAINERS_IMAGE_USER=vscode",
		"USER $_DEV_CONTAINERS_IMAGE_USER",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(string(df), want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}

	// Per-feature dirs populated with run.sh, feature.env, install.sh.
	for _, idx := range []string{"0", "1"} {
		for _, name := range []string{"run.sh", "feature.env", "install.sh", "devcontainer-feature.json"} {
			if _, err := os.Stat(filepath.Join(dst, "build-context", idx, name)); err != nil {
				t.Errorf("build-context/%s/%s missing: %v", idx, name, err)
			}
		}
	}

	// run.sh wraps the install with env-sourcing.
	runSh, _ := os.ReadFile(filepath.Join(dst, "build-context", "0", "run.sh"))
	if !strings.Contains(string(runSh), ". ../builtin.env") {
		t.Errorf("run.sh missing builtin.env source: %q", runSh)
	}
	if !strings.Contains(string(runSh), ". ./feature.env") {
		t.Errorf("run.sh missing feature.env source: %q", runSh)
	}

	// feature.env serialized options.
	envFile, _ := os.ReadFile(filepath.Join(dst, "build-context", "0", "feature.env"))
	if !strings.Contains(string(envFile), "VERSION='1.0'") {
		t.Errorf("feature.env missing VERSION: %q", envFile)
	}
}

func TestGenerateBuildContext_AlreadyInstalledSkipped(t *testing.T) {
	dst := t.TempDir()
	already := fixtureFeature(t, "git", "1.0")
	already.AlreadyInstalled = true
	already.Dir = "" // simulate not fetched

	plan := BuildPlan{
		BaseImage: "alpine:3.20",
		Features: []config.ResolvedFeature{
			already,
			fixtureFeature(t, "node", "20"),
		},
	}
	if err := GenerateBuildContext(plan, dst); err != nil {
		t.Fatalf("GenerateBuildContext: %v", err)
	}

	df, _ := os.ReadFile(filepath.Join(dst, "Dockerfile"))
	if !strings.Contains(string(df), "Feature 0: ghcr.io/test/git:1.0 (already installed in base image; skipped)") {
		t.Errorf("expected skip comment for already-installed feature, got\n%s", df)
	}
	if strings.Contains(string(df), "RUN cd /tmp/dc-features/0") {
		t.Errorf("already-installed feature should not have a RUN step:\n%s", df)
	}
	if !strings.Contains(string(df), "RUN cd /tmp/dc-features/1") {
		t.Errorf("normal feature missing RUN step:\n%s", df)
	}

	// 0/ dir not created.
	if _, err := os.Stat(filepath.Join(dst, "build-context", "0")); err == nil {
		t.Error("build-context/0 should not exist for already-installed feature")
	}
}

func TestGenerateBuildContext_RejectsUnfetchedFeature(t *testing.T) {
	dst := t.TempDir()
	plan := BuildPlan{
		BaseImage: "alpine:3.20",
		Features: []config.ResolvedFeature{
			{Ref: "ghcr.io/x/y:1", SourceKind: config.FeatureSourceOCI},
		},
	}
	if err := GenerateBuildContext(plan, dst); err == nil {
		t.Fatal("expected error for unfetched feature")
	}
}

func TestBuildPlan_HasWork(t *testing.T) {
	if (BuildPlan{}).HasWork() {
		t.Error("empty plan should not have work")
	}
	full := BuildPlan{Features: []config.ResolvedFeature{fixtureFeature(t, "x", "1")}}
	if !full.HasWork() {
		t.Error("plan with one feature should have work")
	}
	allInstalled := BuildPlan{Features: []config.ResolvedFeature{
		{AlreadyInstalled: true},
		{AlreadyInstalled: true},
	}}
	if allInstalled.HasWork() {
		t.Error("plan with all-already-installed should not have work")
	}
}

func TestBuildMetadataLabel_Shape(t *testing.T) {
	plan := BuildPlan{
		BaseImage:  "alpine:3.20",
		RemoteUser: "vscode",
		Features: []config.ResolvedFeature{
			fixtureFeature(t, "git", "1.0"),
		},
	}
	got, err := buildMetadataLabel(plan)
	if err != nil {
		t.Fatalf("buildMetadataLabel: %v", err)
	}
	// Feature entry + final config entry.
	str := string(got)
	if !strings.HasPrefix(str, "[") || !strings.HasSuffix(str, "]") {
		t.Errorf("expected JSON array, got %s", str)
	}
	if !strings.Contains(str, `"id":"git"`) {
		t.Errorf("expected git id in label: %s", str)
	}
	if !strings.Contains(str, `"resolvedRef":"ghcr.io/test/git@sha256:deadbeef"`) {
		t.Errorf("expected resolved digest in label: %s", str)
	}
	if !strings.Contains(str, `"remoteUser":"vscode"`) {
		t.Errorf("expected final config remoteUser entry in label: %s", str)
	}
}

func TestParseMetadataLabel_RoundTrip(t *testing.T) {
	plan := BuildPlan{
		BaseImage: "alpine:3.20",
		Features: []config.ResolvedFeature{
			fixtureFeature(t, "git", "1.0"),
			fixtureFeature(t, "node", "20"),
		},
	}
	raw, _ := buildMetadataLabel(plan)
	parsed, err := ParseMetadataLabel(string(raw))
	if err != nil {
		t.Fatalf("ParseMetadataLabel: %v", err)
	}
	// Expect the two feature entries plus the final no-ID resolved-config
	// entry — the latter carries user mergeable overrides and must not be
	// dropped on round-trip.
	if len(parsed) != 3 {
		t.Fatalf("expected 3 entries back (2 features + final config), got %d (raw=%s)", len(parsed), raw)
	}
	if parsed[0].ID != "git" || parsed[1].ID != "node" {
		t.Errorf("got %+v", parsed)
	}
	if parsed[2].ID != "" {
		t.Errorf("expected final entry to have no ID, got %q", parsed[2].ID)
	}
}

func TestParseMetadataLabel_PreservesFinalEntryFields(t *testing.T) {
	// Reference behavior: the final no-ID resolved-config entry carries
	// remoteUser/containerUser. ParseMetadataLabel must round-trip them
	// — this is the primary symptom of issue #20.
	label := `[{"id":"common-utils","version":"2"},{"remoteUser":"vscode","containerUser":"vscode"}]`
	parsed, err := ParseMetadataLabel(label)
	if err != nil {
		t.Fatalf("ParseMetadataLabel: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("want 2 entries, got %d", len(parsed))
	}
	if parsed[1].RemoteUser != "vscode" || parsed[1].ContainerUser != "vscode" {
		t.Errorf("final entry: got remoteUser=%q containerUser=%q", parsed[1].RemoteUser, parsed[1].ContainerUser)
	}
}

func TestParseMetadataLabel_AllFieldsRoundTrip(t *testing.T) {
	label := `[{
		"id":"x",
		"remoteUser":"u",
		"containerUser":"u",
		"userEnvProbe":"none",
		"waitFor":"postStart",
		"shutdownAction":"stopContainer",
		"updateRemoteUserUID":true,
		"init":true,
		"privileged":false,
		"overrideCommand":false,
		"containerEnv":{"K":"v"},
		"remoteEnv":{"R":"e"},
		"capAdd":["SYS_PTRACE"],
		"securityOpt":["seccomp=unconfined"],
		"entrypoint":"/bin/x",
		"mounts":[{"type":"bind","source":"/h","target":"/c"}],
		"hostRequirements":{"cpus":2,"memory":"4gb"},
		"postCreateCommand":"echo hi"
	}]`
	parsed, err := ParseMetadataLabel(label)
	if err != nil {
		t.Fatalf("ParseMetadataLabel: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("want 1 entry, got %d", len(parsed))
	}
	e := parsed[0]
	if e.RemoteUser != "u" || e.ContainerUser != "u" || e.UserEnvProbe != "none" {
		t.Errorf("scalar fields: %+v", e)
	}
	if e.UpdateRemoteUserUID == nil || !*e.UpdateRemoteUserUID {
		t.Errorf("UpdateRemoteUserUID = %v", e.UpdateRemoteUserUID)
	}
	if e.Init == nil || !*e.Init {
		t.Errorf("Init = %v", e.Init)
	}
	if len(e.Mounts) != 1 || e.Mounts[0].Target != "/c" {
		t.Errorf("Mounts = %+v", e.Mounts)
	}
	if e.HostRequirements == nil || e.HostRequirements.CPUs != 2 {
		t.Errorf("HostRequirements = %+v", e.HostRequirements)
	}
	if e.PostCreateCommand.Single == nil || e.PostCreateCommand.Single.Shell != "echo hi" {
		t.Errorf("PostCreateCommand = %+v", e.PostCreateCommand)
	}
}

func TestParseMetadataLabel_Empty(t *testing.T) {
	got, err := ParseMetadataLabel("")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}
