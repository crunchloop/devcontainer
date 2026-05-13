package config

import (
	"encoding/json"
	"reflect"
	"strings"
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
	// Pre-Finalize: optional fields stay unset.
	if cfg.OverrideCommand != nil {
		t.Errorf("OverrideCommand should be nil pre-Finalize, got %+v", cfg.OverrideCommand)
	}
	if cfg.UserEnvProbe != "" {
		t.Errorf("UserEnvProbe should be empty pre-Finalize, got %q", cfg.UserEnvProbe)
	}
	if cfg.WaitFor != "" {
		t.Errorf("WaitFor should be empty pre-Finalize, got %q", cfg.WaitFor)
	}
	cfg.Finalize()
	if !BoolOr(cfg.OverrideCommand, false) {
		t.Error("OverrideCommand should default to true after Finalize")
	}
	if cfg.UserEnvProbe != UserEnvProbeLoginInteractive {
		t.Errorf("UserEnvProbe = %q, want loginInteractiveShell after Finalize", cfg.UserEnvProbe)
	}
	if cfg.WaitFor != LifecyclePostCreate {
		t.Errorf("WaitFor = %q, want postCreate after Finalize (no updateContent set)", cfg.WaitFor)
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
	if len(cfg.Lifecycle.UpdateContent) != 1 ||
		cfg.Lifecycle.UpdateContent[0].Single == nil ||
		cfg.Lifecycle.UpdateContent[0].Single.Shell != "git submodule update --init" {
		t.Errorf("UpdateContent = %+v", cfg.Lifecycle.UpdateContent)
	}
	if len(cfg.Lifecycle.PostCreate) != 1 ||
		cfg.Lifecycle.PostCreate[0].Single == nil ||
		!reflect.DeepEqual(cfg.Lifecycle.PostCreate[0].Single.Exec, []string{"bash", "-c", "setup.sh"}) {
		t.Errorf("PostCreate = %+v", cfg.Lifecycle.PostCreate)
	}
	cfg.Finalize()
	if cfg.WaitFor != LifecycleUpdateContent {
		t.Errorf("WaitFor = %q, want updateContent after Finalize (updateContent set => default switches)", cfg.WaitFor)
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

func TestResolveBytes_FeaturesPartial(t *testing.T) {
	// Resolve populates Features partially: Ref, Options, SourceKind set;
	// fetched fields (Dir, Metadata, ResolvedRef) empty until the engine
	// build path runs. AlreadyInstalled is false until the metadata label
	// read in the engine flips it.
	cfg := resolveJSON(t, `{
		"image":"alpine",
		"features": {
			"ghcr.io/devcontainers/features/node:1": {"version": "lts"},
			"ghcr.io/devcontainers/features/git:1": "2.40.0",
			"./local-feature": {}
		}
	}`, ResolveInput{})

	if len(cfg.Features) != 3 {
		t.Fatalf("Features count = %d, want 3", len(cfg.Features))
	}

	byRef := make(map[string]ResolvedFeature, len(cfg.Features))
	for _, f := range cfg.Features {
		byRef[f.Ref] = f
	}

	node := byRef["ghcr.io/devcontainers/features/node:1"]
	if node.SourceKind != FeatureSourceOCI {
		t.Errorf("node SourceKind = %q", node.SourceKind)
	}
	if node.Options["version"] != "lts" {
		t.Errorf("node options = %+v", node.Options)
	}
	if node.Dir != "" || node.Metadata.ID != "" {
		t.Errorf("node should have empty fetched fields, got Dir=%q Metadata=%+v", node.Dir, node.Metadata)
	}

	git := byRef["ghcr.io/devcontainers/features/git:1"]
	if git.Options["version"] != "2.40.0" {
		t.Errorf("git shorthand-version not parsed: %+v", git.Options)
	}

	local := byRef["./local-feature"]
	if local.SourceKind != FeatureSourceLocal {
		t.Errorf("local SourceKind = %q", local.SourceKind)
	}

	for _, w := range cfg.Warnings {
		if w.Code == WarnUnsupportedFeatureField {
			t.Errorf("WarnUnsupportedFeatureField should no longer appear: %v", w)
		}
	}
}

func TestResolveBytes_OverrideFeatureInstallOrder(t *testing.T) {
	cfg := resolveJSON(t, `{
		"image":"alpine",
		"features": {
			"ghcr.io/devcontainers/features/node:1": {},
			"ghcr.io/devcontainers/features/git:1": {},
			"ghcr.io/devcontainers/features/python:3": {}
		},
		"overrideFeatureInstallOrder": [
			"ghcr.io/devcontainers/features/git",
			"ghcr.io/devcontainers/features/node"
		]
	}`, ResolveInput{})

	if len(cfg.Features) != 3 {
		t.Fatalf("Features count = %d", len(cfg.Features))
	}
	wantOrder := []string{
		"ghcr.io/devcontainers/features/git:1",
		"ghcr.io/devcontainers/features/node:1",
		"ghcr.io/devcontainers/features/python:3",
	}
	for i, want := range wantOrder {
		if cfg.Features[i].Ref != want {
			t.Errorf("Features[%d].Ref = %q, want %q", i, cfg.Features[i].Ref, want)
		}
	}
}

func TestResolveBytes_Customizations(t *testing.T) {
	// Tool-namespaced customizations are passed through as
	// json.RawMessage. Each consumer decodes its own namespace; the
	// engine itself never interprets these. We use "acme" here as a
	// placeholder for any caller-defined namespace.
	cfg := resolveJSON(t, `{
		"image":"alpine",
		"customizations": {
			"acme": {"hooks":["./script.sh"]},
			"vscode": {"extensions":["golang.go"]}
		}
	}`, ResolveInput{})
	if len(cfg.Customizations) != 2 {
		t.Fatalf("Customizations count = %d", len(cfg.Customizations))
	}
	acme := cfg.Customizations["acme"]
	var acmeDecoded struct {
		Hooks []string `json:"hooks"`
	}
	if err := json.Unmarshal(acme, &acmeDecoded); err != nil {
		t.Fatalf("acme decode: %v", err)
	}
	if !reflect.DeepEqual(acmeDecoded.Hooks, []string{"./script.sh"}) {
		t.Errorf("Hooks = %+v", acmeDecoded.Hooks)
	}
}

func TestResolveBytes_ComposePortsIgnored(t *testing.T) {
	cfg := resolveJSON(t, `{
		"dockerComposeFile": "compose.yml",
		"service": "app",
		"forwardPorts": [3000, 8080]
	}`, ResolveInput{})
	found := false
	for _, w := range cfg.Warnings {
		if w.Code == WarnComposePortsIgnored && w.Path == "/forwardPorts" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected WarnComposePortsIgnored on compose source with forwardPorts, got %v", cfg.Warnings)
	}
}

func TestResolveBytes_ComposeWithoutPortsNoWarning(t *testing.T) {
	cfg := resolveJSON(t, `{
		"dockerComposeFile": "compose.yml",
		"service": "app"
	}`, ResolveInput{})
	for _, w := range cfg.Warnings {
		if w.Code == WarnComposePortsIgnored {
			t.Errorf("unexpected WarnComposePortsIgnored: %v", w)
		}
	}
}

func TestResolveBytes_ImageWithPortsNoWarning(t *testing.T) {
	// Image source with forwardPorts is fine — we'd actuate them in
	// a future PR (currently informational on all source kinds, but
	// only compose explicitly warns about it).
	cfg := resolveJSON(t, `{
		"image":"alpine",
		"forwardPorts":[3000]
	}`, ResolveInput{})
	for _, w := range cfg.Warnings {
		if w.Code == WarnComposePortsIgnored {
			t.Errorf("ComposePortsIgnored should not fire on image source: %v", w)
		}
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

func TestResolveBytes_AppPortTranslatesIntoForwardPorts(t *testing.T) {
	cases := []struct {
		name string
		json string
		want []PortSpec
	}{
		{
			name: "scalar int",
			json: `{"image":"alpine","appPort":3000}`,
			want: []PortSpec{{Container: 3000}},
		},
		{
			name: "scalar host:container string",
			json: `{"image":"alpine","appPort":"8080:80"}`,
			want: []PortSpec{{Host: 8080, Container: 80}},
		},
		{
			name: "array of mixed forms",
			json: `{"image":"alpine","appPort":[3000,"4000","5000:5000"]}`,
			want: []PortSpec{
				{Container: 3000},
				{Container: 4000},
				{Host: 5000, Container: 5000},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resolveJSON(t, tc.json, ResolveInput{})
			if len(cfg.ForwardPorts) != len(tc.want) {
				t.Fatalf("ForwardPorts = %+v, want %+v", cfg.ForwardPorts, tc.want)
			}
			for i, w := range tc.want {
				if cfg.ForwardPorts[i] != w {
					t.Errorf("ForwardPorts[%d] = %+v, want %+v", i, cfg.ForwardPorts[i], w)
				}
			}
			// Deprecation warning should mention translation.
			var dep *Warning
			for i := range cfg.Warnings {
				if cfg.Warnings[i].Code == WarnDeprecatedKey && cfg.Warnings[i].Path == "/appPort" {
					dep = &cfg.Warnings[i]
				}
			}
			if dep == nil {
				t.Fatalf("missing /appPort deprecation warning")
				return
			}
			if !strings.Contains(dep.Message, "translated") {
				t.Errorf("warning should note translation, got %q", dep.Message)
			}
		})
	}
}

func TestResolveBytes_AppPortDoesNotOverrideForwardPorts(t *testing.T) {
	// When the same container port is already in forwardPorts (with an
	// explicit host binding), appPort must not duplicate or override it.
	cfg := resolveJSON(t, `{
		"image":"alpine",
		"forwardPorts":["9000:3000"],
		"appPort":[3000,4000]
	}`, ResolveInput{})

	want := []PortSpec{
		{Host: 9000, Container: 3000}, // from forwardPorts, preserved
		{Container: 4000},             // from appPort, newly added
	}
	if len(cfg.ForwardPorts) != len(want) {
		t.Fatalf("ForwardPorts = %+v, want %+v", cfg.ForwardPorts, want)
	}
	for i, w := range want {
		if cfg.ForwardPorts[i] != w {
			t.Errorf("ForwardPorts[%d] = %+v, want %+v", i, cfg.ForwardPorts[i], w)
		}
	}
}

func TestResolveBytes_AppPortInvalidItemWarns(t *testing.T) {
	cfg := resolveJSON(t, `{"image":"alpine","appPort":["not-a-port"]}`, ResolveInput{})
	var hasItemWarn bool
	for _, w := range cfg.Warnings {
		if w.Path == "/appPort/0" {
			hasItemWarn = true
		}
	}
	if !hasItemWarn {
		t.Errorf("expected per-item warning at /appPort/0, got %v", cfg.Warnings)
	}
	if len(cfg.ForwardPorts) != 0 {
		t.Errorf("expected no translated entries on parse failure, got %+v", cfg.ForwardPorts)
	}
}

func TestResolveBytes_OverrideCommandFalse(t *testing.T) {
	cfg := resolveJSON(t, `{"image":"alpine","overrideCommand":false}`, ResolveInput{})
	cfg.Finalize()
	if BoolOr(cfg.OverrideCommand, true) {
		t.Error("OverrideCommand=false should be honored even after Finalize")
	}
}
