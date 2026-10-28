package devcontainer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/compose"
	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/runtime"
)

// hashFixture is a resolved compose config plus its loaded project, as
// Up has them before any feature is fetched.
type hashFixture struct {
	eng     *Engine
	cfg     *config.ResolvedConfig
	opts    UpOptions
	project *composetypes.Project
}

// newHashFixture writes the workspace into dir (shared between the
// fixtures a test compares, so paths and the devcontainer ID match)
// and resolves it.
func newHashFixture(t *testing.T, dir, devcontainerJSON, composeYAML string) *hashFixture {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, ".devcontainer", "devcontainer.json")
	if err := os.WriteFile(cfgPath, []byte(devcontainerJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := UpOptions{LocalWorkspaceFolder: dir, ConfigPath: cfgPath, LocalEnv: map[string]string{"HOME": "/home/x"}}
	cfg, err := Resolve(context.Background(), ResolveOptions{LocalWorkspaceFolder: dir, ConfigPath: cfgPath, LocalEnv: opts.LocalEnv})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	src := cfg.Source.(*config.ComposeSource)
	project, err := compose.Load(context.Background(), compose.LoadOptions{
		Files:       src.Files,
		WorkingDir:  composeWorkingDir(cfg, opts),
		ProjectName: composeProjectName(cfg),
		Env:         mapToEnvList(opts.LocalEnv),
	})
	if err != nil {
		t.Fatalf("compose.Load: %v", err)
	}
	eng, err := New(EngineOptions{Runtime: newFakeRuntime(), ComposeBackend: ComposeBackendNative})
	if err != nil {
		t.Fatal(err)
	}
	return &hashFixture{eng: eng, cfg: cfg, opts: opts, project: project}
}

func (f *hashFixture) hash(t *testing.T, baseID string) string {
	t.Helper()
	h, err := f.eng.composeInputsHash(f.cfg, f.opts, f.project, baseID)
	if err != nil {
		t.Fatalf("composeInputsHash: %v", err)
	}
	return h
}

const hashBaseJSON = `{
	"dockerComposeFile": "../docker-compose.yml",
	"service": "app",
	"workspaceFolder": "/workspaces/proj",
	"features": { "ghcr.io/devcontainers/features/docker-in-docker:2": { "moby": false } },
	"postStartCommand": "echo hi",
	"customizations": { "vscode": { "extensions": ["a.b"] } }
}`

const hashBaseCompose = `
services:
  app:
    image: debian:bookworm-slim
    command: ["sleep", "infinity"]
  db:
    image: postgres:17
`

func TestComposeInputsHash_Deterministic(t *testing.T) {
	a := newHashFixture(t, t.TempDir(), hashBaseJSON, hashBaseCompose)
	h1 := a.hash(t, "sha256:base")
	if h2 := a.hash(t, "sha256:base"); h1 != h2 {
		t.Fatalf("same inputs hashed differently: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("hash %q is not a sha256 hex digest", h1)
	}
}

// Everything that shapes the primary image or the project changes the
// hash, so a changed input takes the full path (fetch and build).
func TestComposeInputsHash_ChangesWithInputs(t *testing.T) {
	cases := []struct {
		name        string
		json, yaml  string
		baseID      string
		disablePort bool
	}{
		{name: "base image ID", baseID: "sha256:other"},
		{name: "feature option", json: replaceOnce(t, hashBaseJSON, `"moby": false`, `"moby": true`)},
		{name: "feature version", json: replaceOnce(t, hashBaseJSON, "docker-in-docker:2", "docker-in-docker:2.17.0")},
		{name: "containerEnv", json: replaceOnce(t, hashBaseJSON, `"service": "app",`, `"service": "app", "containerEnv": {"A": "1"},`)},
		{name: "mounts", json: replaceOnce(t, hashBaseJSON, `"service": "app",`, `"service": "app", "mounts": ["type=volume,source=v,target=/v"],`)},
		{name: "primary service", yaml: replaceOnce(t, hashBaseCompose, `["sleep", "infinity"]`, `["sleep", "1000"]`)},
		{name: "sidecar service", yaml: replaceOnce(t, hashBaseCompose, "postgres:17", "postgres:18")},
		{name: "engine option", disablePort: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			want := newHashFixture(t, dir, hashBaseJSON, hashBaseCompose).hash(t, "sha256:base")
			json, yaml, baseID := hashBaseJSON, hashBaseCompose, "sha256:base"
			if c.json != "" {
				json = c.json
			}
			if c.yaml != "" {
				yaml = c.yaml
			}
			if c.baseID != "" {
				baseID = c.baseID
			}
			f := newHashFixture(t, dir, json, yaml)
			f.eng.opts.DisableHostPortPublishing = c.disablePort
			if got := f.hash(t, baseID); got == want {
				t.Fatalf("hash unchanged after changing the %s", c.name)
			}
		})
	}
}

// Fields Up re-reads from devcontainer.json on every call and that never
// reach the container or its image do not change the hash: editing a
// lifecycle command must not cost a feature fetch and a rebuild.
func TestComposeInputsHash_IgnoresNonContainerFields(t *testing.T) {
	dir := t.TempDir()
	want := newHashFixture(t, dir, hashBaseJSON, hashBaseCompose).hash(t, "sha256:base")

	edited := replaceOnce(t, hashBaseJSON, `"postStartCommand": "echo hi"`,
		`"name": "renamed", "postStartCommand": "echo bye", "remoteEnv": {"X": "y"}, "forwardPorts": [3000]`)
	edited = replaceOnce(t, edited, `["a.b"]`, `["c.d"]`)
	if got := newHashFixture(t, dir, edited, hashBaseCompose).hash(t, "sha256:base"); got != want {
		t.Fatalf("hash changed after editing name/lifecycle/remoteEnv/ports/customizations")
	}
}

func TestComposeInputsHash_LocalFeatureContent(t *testing.T) {
	dir := t.TempDir()
	json := replaceOnce(t, hashBaseJSON, `"ghcr.io/devcontainers/features/docker-in-docker:2": { "moby": false }`, `"./local-feature": {}`)
	featDir := filepath.Join(dir, ".devcontainer", "local-feature")
	if err := os.MkdirAll(featDir, 0o755); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(featDir, "install.sh")
	if err := os.WriteFile(install, []byte("#!/bin/sh\necho one\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newHashFixture(t, dir, json, hashBaseCompose)
	h1 := f.hash(t, "sha256:base")
	if err := os.WriteFile(install, []byte("#!/bin/sh\necho two\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if h2 := f.hash(t, "sha256:base"); h1 == h2 {
		t.Fatal("hash unchanged after editing a local feature's install.sh")
	}
}

func TestComposeInputsHash_RequiresBaseImageID(t *testing.T) {
	f := newHashFixture(t, t.TempDir(), hashBaseJSON, hashBaseCompose)
	if _, err := f.eng.composeInputsHash(f.cfg, f.opts, f.project, ""); err == nil {
		t.Fatal("hash computed without a base image ID")
	}
}

func replaceOnce(t *testing.T, s, old, new string) string {
	t.Helper()
	if strings.Count(s, old) != 1 {
		t.Fatalf("%q does not occur exactly once", old)
	}
	return strings.Replace(s, old, new, 1)
}

// listingFake answers ListContainers from the fakeRuntime's containers.
type listingFake struct{ *fakeRuntime }

func (l listingFake) ListContainers(ctx context.Context, filter runtime.LabelFilter) ([]runtime.Container, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []runtime.Container
	for _, d := range l.containersByID {
		ok := true
		for k, v := range filter.Match {
			if d.Labels[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, d.Container)
		}
	}
	return out, nil
}

// reattachMissFixture is a compose workspace whose primary container
// exists with the hash of the current inputs; each test breaks one
// precondition and expects a miss (so Up falls back to the full path)
// without cfg being touched.
func reattachMissFixture(t *testing.T) (*hashFixture, *fakeRuntime, *runtime.ContainerDetails) {
	t.Helper()
	f := newHashFixture(t, t.TempDir(), hashBaseJSON, hashBaseCompose)
	fr := newFakeRuntime()
	eng, err := New(EngineOptions{Runtime: listingFake{fr}, ComposeBackend: ComposeBackendNative})
	if err != nil {
		t.Fatal(err)
	}
	f.eng = eng
	f.opts.bus = newEventBus(events.NewEmitter(nil), nil)
	fr.imagesByRef["debian:bookworm-slim"] = &runtime.ImageDetails{ID: "sha256:base"}
	fr.imagesByRef["dc-go-final-x:latest"] = &runtime.ImageDetails{ID: "sha256:final"}
	c := &runtime.ContainerDetails{
		Container: runtime.Container{ID: "c1", Image: "sha256:final", State: runtime.StateExited},
		ImageRef:  "dc-go-final-x:latest",
		Labels: map[string]string{
			compose.LabelComposeProject: composeProjectName(f.cfg),
			compose.LabelComposeService: "app",
			LabelDevcontainerID:         f.cfg.DevcontainerID,
			LabelComposeInputsHash:      f.hash(t, "sha256:base"),
		},
	}
	fr.containersByID["c1"] = c
	return f, fr, c
}

func (f *hashFixture) reattach(t *testing.T) error {
	t.Helper()
	src := f.cfg.Source.(*config.ComposeSource)
	primary, err := compose.PrimaryService(f.project, src.Service)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.eng.reattachCompose(context.Background(), f.cfg, f.opts, f.project, src, primary,
		composeProjectName(f.cfg), composeWorkingDir(f.cfg, f.opts))
	return err
}

func TestReattachCompose_Misses(t *testing.T) {
	// Control: with every precondition holding, reattach commits (here
	// it then fails in the fake's orchestrator primitives, which is not
	// a miss), so each case below misses for the reason it names.
	t.Run("control", func(t *testing.T) {
		f, _, _ := reattachMissFixture(t)
		if err := f.reattach(t); errors.Is(err, errComposeReattachMiss) {
			t.Fatalf("intact fixture missed: %v", err)
		}
		if len(f.cfg.Entrypoints) == 0 && f.cfg.OverrideCommand == nil {
			t.Fatalf("committed reattach did not merge metadata into cfg")
		}
	})
	cases := map[string]func(f *hashFixture, fr *fakeRuntime, c *runtime.ContainerDetails){
		"no hash label (created before reattach existed)": func(_ *hashFixture, _ *fakeRuntime, c *runtime.ContainerDetails) {
			delete(c.Labels, LabelComposeInputsHash)
		},
		"inputs changed": func(_ *hashFixture, _ *fakeRuntime, c *runtime.ContainerDetails) {
			c.Labels[LabelComposeInputsHash] = "0000"
		},
		"base image moved": func(_ *hashFixture, fr *fakeRuntime, _ *runtime.ContainerDetails) {
			fr.imagesByRef["debian:bookworm-slim"] = &runtime.ImageDetails{ID: "sha256:newbase"}
		},
		"base image missing": func(_ *hashFixture, fr *fakeRuntime, _ *runtime.ContainerDetails) {
			delete(fr.imagesByRef, "debian:bookworm-slim")
		},
		"final image tag moved": func(_ *hashFixture, fr *fakeRuntime, _ *runtime.ContainerDetails) {
			fr.imagesByRef["dc-go-final-x:latest"] = &runtime.ImageDetails{ID: "sha256:rebuilt"}
		},
		"no primary container": func(_ *hashFixture, fr *fakeRuntime, _ *runtime.ContainerDetails) {
			delete(fr.containersByID, "c1")
		},
		"pull policy always": func(f *hashFixture, _ *fakeRuntime, _ *runtime.ContainerDetails) {
			f.opts.PullPolicy = PullAlways
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f, fr, c := reattachMissFixture(t)
			breakIt(f, fr, c)
			before := *f.cfg
			err := f.reattach(t)
			if !errors.Is(err, errComposeReattachMiss) {
				t.Fatalf("reattach = %v, want a miss", err)
			}
			if len(f.cfg.Mounts) != len(before.Mounts) || len(f.cfg.Entrypoints) != len(before.Entrypoints) || f.cfg.OverrideCommand != before.OverrideCommand {
				t.Fatalf("cfg mutated on a miss")
			}
		})
	}
}
