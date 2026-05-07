package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

const fakeID = "abcdef0123456789"

func resolveJSON(t *testing.T, src string, input ResolveInput) *ResolvedConfig {
	t.Helper()
	if input.LocalWorkspaceFolder == "" {
		input.LocalWorkspaceFolder = "/home/u/proj"
	}
	if input.ConfigPath == "" {
		input.ConfigPath = "/home/u/proj/.devcontainer/devcontainer.json"
	}
	if input.DevcontainerID == "" {
		input.DevcontainerID = fakeID
	}
	got, err := ResolveBytes([]byte(src), input)
	if err != nil {
		t.Fatalf("ResolveBytes: %v", err)
	}
	return got
}

func TestResolveBytes_ImageMinimal(t *testing.T) {
	cfg := resolveJSON(t, `{"image":"alpine:3.20"}`, ResolveInput{})
	if cfg.DevcontainerID != fakeID {
		t.Errorf("DevcontainerID = %q", cfg.DevcontainerID)
	}
	img, ok := cfg.Source.(*ImageSource)
	if !ok || img.Image != "alpine:3.20" {
		t.Fatalf("Source = %+v", cfg.Source)
	}
	if cfg.ContainerWorkspaceFolder != "/workspaces/proj" {
		t.Errorf("ContainerWorkspaceFolder = %q", cfg.ContainerWorkspaceFolder)
	}
	if !cfg.OverrideCommand {
		t.Error("OverrideCommand should default to true")
	}
	if cfg.UserEnvProbe != UserEnvProbeLoginInteractive {
		t.Errorf("UserEnvProbe = %q, want loginInteractiveShell", cfg.UserEnvProbe)
	}
	if cfg.WaitFor != LifecyclePostCreate {
		t.Errorf("WaitFor = %q, want postCreate (no updateContent set)", cfg.WaitFor)
	}
}

func TestResolveBytes_BuildSource(t *testing.T) {
	cfg := resolveJSON(t, `{
		"build": {
			"dockerfile": "Dockerfile",
			"context": "..",
			"args": {"VERSION": "1.2.3"},
			"cacheFrom": "registry.example.com/cache"
		}
	}`, ResolveInput{})
	bs, ok := cfg.Source.(*BuildSource)
	if !ok {
		t.Fatalf("Source = %+v", cfg.Source)
	}
	if bs.Dockerfile != "Dockerfile" {
		t.Errorf("Dockerfile = %q", bs.Dockerfile)
	}
	// context "..", configDir "/home/u/proj/.devcontainer" → "/home/u/proj"
	if bs.Context != "/home/u/proj" {
		t.Errorf("Context = %q, want /home/u/proj", bs.Context)
	}
	if bs.Args["VERSION"] != "1.2.3" {
		t.Errorf("Args = %+v", bs.Args)
	}
	if !reflect.DeepEqual(bs.CacheFrom, []string{"registry.example.com/cache"}) {
		t.Errorf("CacheFrom = %+v", bs.CacheFrom)
	}
}

func TestResolveBytes_ComposeSource(t *testing.T) {
	cfg := resolveJSON(t, `{
		"dockerComposeFile": ["docker-compose.yml","docker-compose.override.yml"],
		"service": "app",
		"runServices": ["app","db"]
	}`, ResolveInput{})
	cs, ok := cfg.Source.(*ComposeSource)
	if !ok {
		t.Fatalf("Source = %+v", cfg.Source)
	}
	wantFiles := []string{
		"/home/u/proj/.devcontainer/docker-compose.yml",
		"/home/u/proj/.devcontainer/docker-compose.override.yml",
	}
	if !reflect.DeepEqual(cs.Files, wantFiles) {
		t.Errorf("Files = %v, want %v", cs.Files, wantFiles)
	}
	if cs.Service != "app" {
		t.Errorf("Service = %q", cs.Service)
	}
}

func TestResolveBytes_NoSource(t *testing.T) {
	_, err := ResolveBytes([]byte(`{}`), ResolveInput{
		LocalWorkspaceFolder: "/x",
		ConfigPath:           "/x/.devcontainer/devcontainer.json",
		DevcontainerID:       fakeID,
	})
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestResolveBytes_TwoSources(t *testing.T) {
	_, err := ResolveBytes([]byte(`{"image":"a","build":{"dockerfile":"D"}}`), ResolveInput{
		LocalWorkspaceFolder: "/x",
		ConfigPath:           "/x/.devcontainer/devcontainer.json",
		DevcontainerID:       fakeID,
	})
	if err == nil {
		t.Fatal("expected error for multiple sources")
	}
}

func TestResolveBytes_LifecycleAndWaitFor(t *testing.T) {
	cfg := resolveJSON(t, `{
		"image": "alpine:3.20",
		"updateContentCommand": "git submodule update --init",
		"postCreateCommand": ["bash","-c","setup.sh"]
	}`, ResolveInput{})
	if cfg.Lifecycle.UpdateContent.Single == nil ||
		cfg.Lifecycle.UpdateContent.Single.Shell != "git submodule update --init" {
		t.Errorf("UpdateContent = %+v", cfg.Lifecycle.UpdateContent)
	}
	if cfg.Lifecycle.PostCreate.Single == nil ||
		!reflect.DeepEqual(cfg.Lifecycle.PostCreate.Single.Exec, []string{"bash", "-c", "setup.sh"}) {
		t.Errorf("PostCreate = %+v", cfg.Lifecycle.PostCreate)
	}
	if cfg.WaitFor != LifecycleUpdateContent {
		t.Errorf("WaitFor = %q, want updateContent (set => default switches)", cfg.WaitFor)
	}
}

func TestResolveBytes_WaitForExplicit(t *testing.T) {
	cfg := resolveJSON(t, `{
		"image":"alpine",
		"updateContentCommand":"x",
		"waitFor":"postStart"
	}`, ResolveInput{})
	if cfg.WaitFor != "postStart" {
		t.Errorf("explicit waitFor not honored: %q", cfg.WaitFor)
	}
}

func TestResolveBytes_Substitution(t *testing.T) {
	cfg := resolveJSON(t, `{
		"image": "${localEnv:IMAGE}",
		"workspaceFolder": "/work/${localWorkspaceFolderBasename}",
		"containerEnv": {
			"WORKSPACE": "${containerWorkspaceFolder}",
			"USER_HOME": "${containerEnv:HOME}"
		},
		"runArgs": ["--name","${devcontainerId}"]
	}`, ResolveInput{
		LocalEnv: map[string]string{"IMAGE": "myimage:latest"},
	})
	img := cfg.Source.(*ImageSource)
	if img.Image != "myimage:latest" {
		t.Errorf("Image = %q", img.Image)
	}
	if cfg.ContainerWorkspaceFolder != "/work/proj" {
		t.Errorf("ContainerWorkspaceFolder = %q", cfg.ContainerWorkspaceFolder)
	}
	if cfg.ContainerEnv["WORKSPACE"] != "/work/proj" {
		t.Errorf("WORKSPACE = %q", cfg.ContainerEnv["WORKSPACE"])
	}
	// containerEnv pass-through preserved for the runtime to resolve.
	if cfg.ContainerEnv["USER_HOME"] != "${containerEnv:HOME}" {
		t.Errorf("USER_HOME should be left as literal, got %q", cfg.ContainerEnv["USER_HOME"])
	}
	if !reflect.DeepEqual(cfg.RunArgs, []string{"--name", fakeID}) {
		t.Errorf("RunArgs = %v", cfg.RunArgs)
	}
}

func TestResolveBytes_FeaturesWarning(t *testing.T) {
	cfg := resolveJSON(t, `{
		"image":"alpine",
		"features": {
			"ghcr.io/devcontainers/features/node:1": {}
		}
	}`, ResolveInput{})
	if len(cfg.Features) != 0 {
		t.Errorf("Features should be empty in this milestone, got %+v", cfg.Features)
	}
	found := false
	for _, w := range cfg.Warnings {
		if w.Code == WarnUnsupportedFeatureField {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected WarnUnsupportedFeatureField, got warnings: %v", cfg.Warnings)
	}
}

func TestResolveBytes_Customizations(t *testing.T) {
	cfg := resolveJSON(t, `{
		"image":"alpine",
		"customizations": {
			"dap": {"postCacheBuild":["./script.sh"]},
			"vscode": {"extensions":["golang.go"]}
		}
	}`, ResolveInput{})
	if len(cfg.Customizations) != 2 {
		t.Fatalf("Customizations count = %d", len(cfg.Customizations))
	}
	dap := cfg.Customizations["dap"]
	var dapDecoded struct {
		PostCacheBuild []string `json:"postCacheBuild"`
	}
	if err := json.Unmarshal(dap, &dapDecoded); err != nil {
		t.Fatalf("dap decode: %v", err)
	}
	if !reflect.DeepEqual(dapDecoded.PostCacheBuild, []string{"./script.sh"}) {
		t.Errorf("PostCacheBuild = %+v", dapDecoded.PostCacheBuild)
	}
}

func TestResolveBytes_DeprecatedAppPort(t *testing.T) {
	cfg := resolveJSON(t, `{"image":"alpine","appPort":3000}`, ResolveInput{})
	found := false
	for _, w := range cfg.Warnings {
		if w.Code == WarnDeprecatedKey && w.Path == "/appPort" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected WarnDeprecatedKey for /appPort, got %v", cfg.Warnings)
	}
}

func TestResolveBytes_OverrideCommandFalse(t *testing.T) {
	cfg := resolveJSON(t, `{"image":"alpine","overrideCommand":false}`, ResolveInput{})
	if cfg.OverrideCommand {
		t.Error("OverrideCommand=false should be honored")
	}
}
