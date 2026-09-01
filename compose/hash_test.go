package compose

import (
	"math/rand"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

func strPtr(s string) *string { return &s }

// baseService returns a representative compose service so tests
// can mutate copies independently. Mirrors the probe's baseService
// without the volumes/networks edge cases — those are covered by
// the slice-sensitivity tests.
func baseService() composetypes.ServiceConfig {
	return composetypes.ServiceConfig{
		Name:       "app",
		Image:      "node:20",
		Command:    composetypes.ShellCommand{"npm", "run", "dev"},
		Entrypoint: composetypes.ShellCommand{"/usr/bin/env", "sh", "-c"},
		WorkingDir: "/workspaces/proj",
		User:       "1000:1000",
		Environment: composetypes.MappingWithEquals{
			"NODE_ENV":     strPtr("development"),
			"DEBUG":        strPtr("app:*"),
			"DATABASE_URL": strPtr("postgres://db:5432/app"),
			"PORT":         strPtr("3000"),
			"LOG_LEVEL":    strPtr("info"),
		},
		Labels: composetypes.Labels{
			"com.docker.compose.project": "dc-abc123",
			"com.docker.compose.service": "app",
			"dev.containers.id":          "abc123",
		},
		Ports: []composetypes.ServicePortConfig{
			{Target: 3000, Published: "3000", Protocol: "tcp"},
		},
	}
}

func TestConfigHash_Deterministic(t *testing.T) {
	const iterations = 200
	svc := baseService()
	want := ConfigHash("sha256:abc", svc)
	for i := 0; i < iterations; i++ {
		got := ConfigHash("sha256:abc", baseService())
		if got != want {
			t.Fatalf("iter %d: hash changed %q -> %q", i, want, got)
		}
	}
}

// TestConfigHash_MapOrderIndependent shuffles map insertion order
// to defeat any incidental stability and confirms encoding/json's
// map-key-sort guarantee carries through.
func TestConfigHash_MapOrderIndependent(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	want := ConfigHash("img", baseService())
	for trial := 0; trial < 100; trial++ {
		svc := baseService()
		// Drop into a fresh map with permuted insertion order.
		newEnv := composetypes.MappingWithEquals{}
		keys := []string{"NODE_ENV", "DEBUG", "DATABASE_URL", "PORT", "LOG_LEVEL"}
		r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		for _, k := range keys {
			newEnv[k] = svc.Environment[k]
		}
		svc.Environment = newEnv

		newLabels := composetypes.Labels{}
		lkeys := []string{"com.docker.compose.project", "com.docker.compose.service", "dev.containers.id"}
		r.Shuffle(len(lkeys), func(i, j int) { lkeys[i], lkeys[j] = lkeys[j], lkeys[i] })
		for _, k := range lkeys {
			newLabels[k] = svc.Labels[k]
		}
		svc.Labels = newLabels

		if got := ConfigHash("img", svc); got != want {
			t.Fatalf("trial %d: hash changed under map permutation", trial)
		}
	}
}

// TestConfigHash_SliceOrderSensitive locks in that semantic slice
// order (Ports, Command) DOES change the hash. Compose treats these
// as ordered; we mustn't accidentally canonicalize them.
func TestConfigHash_SliceOrderSensitive(t *testing.T) {
	a := baseService()
	a.Ports = []composetypes.ServicePortConfig{
		{Target: 3000, Published: "3000"},
		{Target: 9229, Published: "9229"},
	}
	b := a
	b.Ports = []composetypes.ServicePortConfig{
		{Target: 9229, Published: "9229"},
		{Target: 3000, Published: "3000"},
	}
	if ConfigHash("img", a) == ConfigHash("img", b) {
		t.Error("Ports slice reorder did not change hash; semantic order must affect it")
	}

	c := baseService()
	c.Command = composetypes.ShellCommand{"sh", "-c", "echo hi"}
	d := baseService()
	d.Command = composetypes.ShellCommand{"-c", "sh", "echo hi"}
	if ConfigHash("img", c) == ConfigHash("img", d) {
		t.Error("Command slice reorder did not change hash; semantic order must affect it")
	}
}

// TestConfigHash_ImageIDAffects locks in image-ID sensitivity —
// recreation-on-image-change is a load-bearing property.
func TestConfigHash_ImageIDAffects(t *testing.T) {
	svc := baseService()
	if ConfigHash("img-a", svc) == ConfigHash("img-b", svc) {
		t.Error("hash insensitive to imageID; image change must trigger recreation")
	}
}

// TestConfigHash_StripsNonRuntimeFields pins the contract that
// fields which don't shape the running container — Build,
// PullPolicy, Scale, Deploy, DependsOn, Profiles — do not affect
// the hash. Editing one of them must not recreate the service.
// Matches docker/compose's hash.go behavior.
func TestConfigHash_StripsNonRuntimeFields(t *testing.T) {
	base := baseService()
	wantHash := ConfigHash("img", base)

	t.Run("Build", func(t *testing.T) {
		svc := baseService()
		svc.Build = &composetypes.BuildConfig{Context: "./different"}
		if got := ConfigHash("img", svc); got != wantHash {
			t.Errorf("Build edit changed hash: %q vs %q", got, wantHash)
		}
	})
	t.Run("PullPolicy", func(t *testing.T) {
		svc := baseService()
		svc.PullPolicy = "always"
		if got := ConfigHash("img", svc); got != wantHash {
			t.Errorf("PullPolicy edit changed hash: %q vs %q", got, wantHash)
		}
	})
	t.Run("Scale", func(t *testing.T) {
		svc := baseService()
		scale := 5
		svc.Scale = &scale
		if got := ConfigHash("img", svc); got != wantHash {
			t.Errorf("Scale edit changed hash: %q vs %q", got, wantHash)
		}
	})
	t.Run("DependsOn", func(t *testing.T) {
		svc := baseService()
		svc.DependsOn = composetypes.DependsOnConfig{
			"other": composetypes.ServiceDependency{Condition: "service_started"},
		}
		if got := ConfigHash("img", svc); got != wantHash {
			t.Errorf("DependsOn edit changed hash: %q vs %q", got, wantHash)
		}
	})
	t.Run("Profiles", func(t *testing.T) {
		svc := baseService()
		svc.Profiles = []string{"dev"}
		if got := ConfigHash("img", svc); got != wantHash {
			t.Errorf("Profiles edit changed hash: %q vs %q", got, wantHash)
		}
	})
	t.Run("Deploy", func(t *testing.T) {
		svc := baseService()
		svc.Deploy = &composetypes.DeployConfig{}
		if got := ConfigHash("img", svc); got != wantHash {
			t.Errorf("Deploy edit changed hash: %q vs %q", got, wantHash)
		}
	})
}

// TestConfigHash_StripsLifecycleHooks pins the second half of the
// hook defence. Plan.Validate refuses these fields before any hash
// is computed, so this is belt-and-braces: a recreation destroys the
// container's writable layer, and a field the orchestrator never
// executes must not be able to trigger that even if the refusal is
// moved or a caller reaches ConfigHash directly. Deliberately
// stricter than docker/compose, which executes the hooks.
func TestConfigHash_StripsLifecycleHooks(t *testing.T) {
	base := baseService()
	wantHash := ConfigHash("img", base)
	hook := []composetypes.ServiceHook{{Command: composetypes.ShellCommand{"migrate"}}}

	t.Run("PreStart", func(t *testing.T) {
		svc := baseService()
		svc.PreStart = hook
		if got := ConfigHash("img", svc); got != wantHash {
			t.Errorf("PreStart edit changed hash: %q vs %q", got, wantHash)
		}
	})
	t.Run("PostStart", func(t *testing.T) {
		svc := baseService()
		svc.PostStart = hook
		if got := ConfigHash("img", svc); got != wantHash {
			t.Errorf("PostStart edit changed hash: %q vs %q", got, wantHash)
		}
	})
	t.Run("PreStop", func(t *testing.T) {
		svc := baseService()
		svc.PreStop = hook
		if got := ConfigHash("img", svc); got != wantHash {
			t.Errorf("PreStop edit changed hash: %q vs %q", got, wantHash)
		}
	})
}
