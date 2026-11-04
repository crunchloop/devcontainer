package devcontainer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/feature"
	"github.com/crunchloop/devcontainer/runtime"
)

// depStore is a feature.Store serving OCI features from a table keyed
// by repository. A tag resolves to the entry's digest; repo@digest is
// served when it is that digest.
type depStore struct {
	dir      string
	features map[string]depEntry // repo → entry
	fetched  []string
}

type depEntry struct {
	digest string
	meta   config.FeatureMetadata
}

func (s *depStore) Fetch(_ context.Context, ref string, _ config.FeatureSourceKind) (feature.Fetched, error) {
	s.fetched = append(s.fetched, ref)
	repo := ref
	if i := strings.LastIndex(repo, "@"); i >= 0 {
		repo = repo[:i]
	} else if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	e, ok := s.features[repo]
	if !ok {
		return feature.Fetched{}, errors.New("not found: " + ref)
	}
	return feature.Fetched{Dir: s.dir, ResolvedRef: repo + "@" + e.digest, Metadata: e.meta}, nil
}

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// meta returns feature metadata with id, a "flavor" option defaulting
// to "plain", and the given dependsOn.
func meta(id string, dependsOn map[string]map[string]any) config.FeatureMetadata {
	return config.FeatureMetadata{
		ID:        id,
		Version:   "1.0.0",
		Options:   map[string]config.FeatureOption{"flavor": {Type: "string", Default: "plain"}},
		DependsOn: dependsOn,
	}
}

type depFixture struct {
	t     *testing.T
	eng   *Engine
	rt    *fakeRuntime
	store *depStore
	ws    string
}

func newDepFixture(t *testing.T, features string, table map[string]depEntry) *depFixture {
	t.Helper()
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20","features":`+features+`}`)
	rt := newFakeRuntime()
	store := &depStore{dir: t.TempDir(), features: table}
	eng, err := New(EngineOptions{Runtime: rt, FeatureStore: store})
	if err != nil {
		t.Fatal(err)
	}
	return &depFixture{t: t, eng: eng, rt: rt, store: store, ws: ws}
}

// layer resolves the workspace and runs layerFeatures, returning the
// config it fetched and ordered features into. The fake runtime cannot
// build, so a run that got that far ends in ErrNotImplemented, which is
// swallowed.
func (f *depFixture) layer(mode LockfileMode) (*config.ResolvedConfig, error) {
	f.t.Helper()
	configPath := filepath.Join(f.ws, ".devcontainer", "devcontainer.json")
	cfg, err := Resolve(context.Background(), ResolveOptions{LocalWorkspaceFolder: f.ws, ConfigPath: configPath})
	if err != nil {
		f.t.Fatal(err)
	}
	opts := UpOptions{LocalWorkspaceFolder: f.ws, ConfigPath: configPath, Lockfile: mode}
	opts.bus = newEventBus(f.eng.emitter, nil)
	defer opts.bus.Close()
	_, _, err = f.eng.layerFeatures(context.Background(), cfg, "alpine:3.20", opts)
	if errors.Is(err, runtime.ErrNotImplemented) {
		err = nil
	}
	return cfg, err
}

// installed renders cfg.Features in install order as ref or
// ref{flavor} when installed with a non-default flavor; skipped ones
// are suffixed "(baked)".
func installed(cfg *config.ResolvedConfig) []string {
	var out []string
	for _, f := range cfg.Features {
		s := f.Ref
		if fl, _ := f.Options["flavor"].(string); fl != "" && fl != "plain" {
			s += "{" + fl + "}"
		}
		if f.AlreadyInstalled {
			s += "(baked)"
		}
		out = append(out, s)
	}
	return out
}

func TestDependsOn_PullsInChainInDependencyOrder(t *testing.T) {
	f := newDepFixture(t, `{"ghcr.io/x/a:1":{}}`, map[string]depEntry{
		"ghcr.io/x/a": {digestOf('a'), meta("a", map[string]map[string]any{"ghcr.io/x/b:1": {}})},
		"ghcr.io/x/b": {digestOf('b'), meta("b", map[string]map[string]any{"ghcr.io/x/c:1": {}})},
		"ghcr.io/x/c": {digestOf('c'), meta("c", nil)},
	})
	cfg, err := f.layer(LockfileDefault)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := installed(cfg), []string{"ghcr.io/x/c:1", "ghcr.io/x/b:1", "ghcr.io/x/a:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("installed %v, want %v", got, want)
	}
	// Pulled-in features get spec defaults like declared ones.
	if fl := cfg.Features[0].Options["flavor"]; fl != "plain" {
		t.Errorf("pulled-in feature options = %v, want defaults applied", cfg.Features[0].Options)
	}
}

func TestDependsOn_DiamondInstallsSharedDependencyOnce(t *testing.T) {
	f := newDepFixture(t, `{"ghcr.io/x/a:1":{}}`, map[string]depEntry{
		"ghcr.io/x/a": {digestOf('a'), meta("a", map[string]map[string]any{"ghcr.io/x/b:1": {}, "ghcr.io/x/c:1": {}})},
		"ghcr.io/x/b": {digestOf('b'), meta("b", map[string]map[string]any{"ghcr.io/x/d:1": {}})},
		"ghcr.io/x/c": {digestOf('c'), meta("c", map[string]map[string]any{"ghcr.io/x/d:1": {}})},
		"ghcr.io/x/d": {digestOf('d'), meta("d", nil)},
	})
	cfg, err := f.layer(LockfileDefault)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := installed(cfg), []string{"ghcr.io/x/d:1", "ghcr.io/x/b:1", "ghcr.io/x/c:1", "ghcr.io/x/a:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("installed %v, want %v", got, want)
	}
	if n := strings.Count(strings.Join(f.store.fetched, " "), "ghcr.io/x/d"); n != 1 {
		t.Errorf("fetched d %d times: %v", n, f.store.fetched)
	}
}

// TestDependsOn_DeclaredFeatureSatisfiesDependency covers spec
// equality: a dependency is a declared feature when both are the same
// content with the same options once defaults apply, however the
// reference is spelled, and a different feature (installed again)
// when the options differ.
func TestDependsOn_DeclaredFeatureSatisfiesDependency(t *testing.T) {
	table := func(depOpts map[string]any, depKey string) map[string]depEntry {
		return map[string]depEntry{
			"ghcr.io/x/a": {digestOf('a'), meta("a", map[string]map[string]any{depKey: depOpts})},
			"ghcr.io/x/b": {digestOf('b'), meta("b", nil)},
		}
	}
	for _, tc := range []struct {
		name    string
		depKey  string
		depOpts map[string]any
		want    []string
	}{
		{"same ref and options", "ghcr.io/x/b:1", map[string]any{},
			[]string{"ghcr.io/x/b:1", "ghcr.io/x/a:1"}},
		{"explicit default", "ghcr.io/x/b:1", map[string]any{"flavor": "plain"},
			[]string{"ghcr.io/x/b:1", "ghcr.io/x/a:1"}},
		{"other tag, same digest", "ghcr.io/x/b:1.0", map[string]any{},
			[]string{"ghcr.io/x/b:1", "ghcr.io/x/a:1"}},
		{"different options", "ghcr.io/x/b:1", map[string]any{"flavor": "spicy"},
			[]string{"ghcr.io/x/b:1", "ghcr.io/x/b:1{spicy}", "ghcr.io/x/a:1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDepFixture(t, `{"ghcr.io/x/a:1":{},"ghcr.io/x/b:1":{}}`, table(tc.depOpts, tc.depKey))
			cfg, err := f.layer(LockfileDefault)
			if err != nil {
				t.Fatal(err)
			}
			if got := installed(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("installed %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDependsOn_CycleThroughPulledInFeature(t *testing.T) {
	f := newDepFixture(t, `{"ghcr.io/x/a:1":{}}`, map[string]depEntry{
		"ghcr.io/x/a": {digestOf('a'), meta("a", map[string]map[string]any{"ghcr.io/x/b:1": {}})},
		"ghcr.io/x/b": {digestOf('b'), meta("b", map[string]map[string]any{"ghcr.io/x/a:1": {}})},
	})
	_, err := f.layer(LockfileDefault)
	var ce *feature.FeatureCycleError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want *FeatureCycleError", err)
	}
	if n := strings.Count(strings.Join(f.store.fetched, " "), "ghcr.io/x/a"); n != 1 {
		t.Errorf("fetched a %d times: %v", n, f.store.fetched)
	}
}

func TestDependsOn_PulledInFeaturesAreLocked(t *testing.T) {
	table := map[string]depEntry{
		"ghcr.io/x/a": {digestOf('a'), meta("a", map[string]map[string]any{"ghcr.io/x/b:1": {}})},
		"ghcr.io/x/b": {digestOf('b'), meta("b", nil)},
	}
	f := newDepFixture(t, `{"ghcr.io/x/a:1":{}}`, table)
	if _, err := f.layer(LockfileDefault); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(f.ws, ".devcontainer", "devcontainer-lock.json")
	lf, err := feature.ReadLockfile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	want := &feature.Lockfile{Features: map[string]feature.LockedFeature{
		"ghcr.io/x/a:1": {Version: "1.0.0", Resolved: "ghcr.io/x/a@" + digestOf('a'), Integrity: digestOf('a'), DependsOn: []string{"ghcr.io/x/b:1"}},
		"ghcr.io/x/b:1": {Version: "1.0.0", Resolved: "ghcr.io/x/b@" + digestOf('b'), Integrity: digestOf('b')},
	}}
	if !lf.Equal(want) {
		t.Errorf("lockfile differs in %v", lf.Changed(want))
	}

	// The pulled-in feature is pinned like a declared one.
	f.store.fetched = nil
	if _, err := f.layer(LockfileFrozen); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ghcr.io/x/a@" + digestOf('a'), "ghcr.io/x/b@" + digestOf('b')}; !reflect.DeepEqual(f.store.fetched, want) {
		t.Errorf("frozen fetched %v, want %v", f.store.fetched, want)
	}

	// A lockfile without the pulled-in feature is out of date.
	delete(lf.Features, "ghcr.io/x/b:1")
	if err := feature.WriteLockfile(lockPath, lf); err != nil {
		t.Fatal(err)
	}
	var me *feature.LockfileMismatchError
	if _, err := f.layer(LockfileFrozen); !errors.As(err, &me) || !reflect.DeepEqual(me.Changed, []string{"ghcr.io/x/b:1"}) {
		t.Fatalf("got %v, want *LockfileMismatchError naming ghcr.io/x/b:1", err)
	}
}

// TestDependsOn_BakedDependency: a dependency the base image's label
// carries at a recorded artifact is not fetched or installed again, but
// its own dependencies are still followed. A label that records them
// says the image has them; one written before they were recorded does
// not, so the baked feature's metadata is read at its artifact to find
// them.
func TestDependsOn_BakedDependency(t *testing.T) {
	table := map[string]depEntry{
		"ghcr.io/x/a": {digestOf('a'), meta("a", map[string]map[string]any{"ghcr.io/x/b:1": {}})},
		"ghcr.io/x/b": {digestOf('b'), meta("b", map[string]map[string]any{"ghcr.io/x/c:1": {}})},
		"ghcr.io/x/c": {digestOf('c'), meta("c", nil)},
	}
	bakedB := `{"id":"b","version":"1.0.0","resolvedRef":"ghcr.io/x/b@` + digestOf('b') + `"`
	bakedC := `{"id":"c","version":"1.0.0","resolvedRef":"ghcr.io/x/c@` + digestOf('c') + `","dependsOn":{}}`
	for _, tc := range []struct {
		name        string
		label       string
		wantFetched []string
		want        []string
	}{
		{
			name:        "label records the dependencies, which the image has",
			label:       `[` + bakedB + `,"dependsOn":{"ghcr.io/x/c:1":{}}},` + bakedC + `]`,
			wantFetched: []string{"ghcr.io/x/a:1"},
			want:        []string{"ghcr.io/x/c:1(baked)", "ghcr.io/x/b:1(baked)", "ghcr.io/x/a:1"},
		},
		{
			name:        "label records the dependencies, which the image lacks",
			label:       `[` + bakedB + `,"dependsOn":{"ghcr.io/x/c:1":{}}}]`,
			wantFetched: []string{"ghcr.io/x/a:1", "ghcr.io/x/c:1"},
			want:        []string{"ghcr.io/x/c:1", "ghcr.io/x/b:1(baked)", "ghcr.io/x/a:1"},
		},
		{
			name:        "label written before dependencies were recorded",
			label:       `[` + bakedB + `}]`,
			wantFetched: []string{"ghcr.io/x/a:1", "ghcr.io/x/b@" + digestOf('b'), "ghcr.io/x/c:1"},
			want:        []string{"ghcr.io/x/c:1", "ghcr.io/x/b:1(baked)", "ghcr.io/x/a:1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDepFixture(t, `{"ghcr.io/x/a:1":{}}`, table)
			f.rt.imagesByRef["alpine:3.20"] = &runtime.ImageDetails{Labels: map[string]string{feature.MetadataLabel: tc.label}}
			cfg, err := f.layer(LockfileDefault)
			if err != nil {
				t.Fatal(err)
			}
			if got := installed(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("installed %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(f.store.fetched, tc.wantFetched) {
				t.Errorf("fetched %v, want %v", f.store.fetched, tc.wantFetched)
			}
		})
	}
}

// TestDependsOn_DuplicateKeyIsLocked: a dependency dropped as the same
// content as a declared feature under another reference is still a
// requested key, so it is locked (to that content) and pinned next time.
func TestDependsOn_DuplicateKeyIsLocked(t *testing.T) {
	f := newDepFixture(t, `{"ghcr.io/x/a:1":{},"ghcr.io/x/b:1":{}}`, map[string]depEntry{
		"ghcr.io/x/a": {digestOf('a'), meta("a", map[string]map[string]any{"ghcr.io/x/b:1.0": {}})},
		"ghcr.io/x/b": {digestOf('b'), meta("b", nil)},
	})
	cfg, err := f.layer(LockfileDefault)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := installed(cfg), []string{"ghcr.io/x/b:1", "ghcr.io/x/a:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("installed %v, want %v", got, want)
	}
	lf, err := feature.ReadLockfile(filepath.Join(f.ws, ".devcontainer", "devcontainer-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := lf.Entry("ghcr.io/x/b:1.0")
	if !ok || entry.Integrity != digestOf('b') {
		t.Fatalf("lock entry for the dependency key = (%+v, %v), want integrity %s", entry, ok, digestOf('b'))
	}
	if _, err := f.layer(LockfileFrozen); err != nil {
		t.Errorf("frozen rerun: %v", err)
	}
}

// TestDependsOn_LocalDependencyOfLocalFeature: a local feature's
// dependsOn on another local feature resolves relative to
// devcontainer.json, as a declared local feature does.
func TestDependsOn_LocalDependencyOfLocalFeature(t *testing.T) {
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20","features":{"./a":{}}}`)
	writeLocalFeature(t, filepath.Join(ws, ".devcontainer", "a"), `{"id":"a","version":"1.0.0","dependsOn":{"./b":{}}}`)
	writeLocalFeature(t, filepath.Join(ws, ".devcontainer", "b"), `{"id":"b","version":"1.0.0"}`)
	eng, err := New(EngineOptions{Runtime: newFakeRuntime()})
	if err != nil {
		t.Fatal(err)
	}
	f := &depFixture{t: t, eng: eng, ws: ws}
	cfg, err := f.layer(LockfileDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := installed(cfg), []string{"./b", "./a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("installed %v, want %v", got, want)
	}
	if want := filepath.Join(ws, ".devcontainer", "b"); cfg.Features[0].Dir != want {
		t.Errorf("./b resolved to %s, want %s", cfg.Features[0].Dir, want)
	}
}

func writeLocalFeature(t *testing.T, dir, metadata string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "devcontainer-feature.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDependsOn_BakedFeatureLockedWithItsDependsOn: a baked feature the
// lockfile does not pin yet is locked from the artifact its label
// records, and its entry carries its dependsOn like a fetched one's:
// from the label, or for an older label from its metadata.
func TestDependsOn_BakedFeatureLockedWithItsDependsOn(t *testing.T) {
	table := map[string]depEntry{
		"ghcr.io/x/b": {digestOf('b'), meta("b", map[string]map[string]any{"ghcr.io/x/c:1": {}})},
		"ghcr.io/x/c": {digestOf('c'), meta("c", nil)},
	}
	bakedB := `{"id":"b","version":"1.0.0","resolvedRef":"ghcr.io/x/b@` + digestOf('b') + `"`
	bakedC := `{"id":"c","version":"1.0.0","resolvedRef":"ghcr.io/x/c@` + digestOf('c') + `","dependsOn":{}}`
	for _, tc := range []struct{ name, label string }{
		{"label records dependsOn", `[` + bakedB + `,"dependsOn":{"ghcr.io/x/c:1":{}}},` + bakedC + `]`},
		{"older label", `[` + bakedB + `},` + bakedC + `]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDepFixture(t, `{"ghcr.io/x/b:1":{}}`, table)
			f.rt.imagesByRef["alpine:3.20"] = &runtime.ImageDetails{Labels: map[string]string{feature.MetadataLabel: tc.label}}
			if _, err := f.layer(LockfileDefault); err != nil {
				t.Fatal(err)
			}
			lf, err := feature.ReadLockfile(filepath.Join(f.ws, ".devcontainer", "devcontainer-lock.json"))
			if err != nil {
				t.Fatal(err)
			}
			entry, ok := lf.Entry("ghcr.io/x/b:1")
			if !ok || entry.Integrity != digestOf('b') || !reflect.DeepEqual(entry.DependsOn, []string{"ghcr.io/x/c:1"}) {
				t.Errorf("lock entry for b = (%+v, %v), want integrity %s and dependsOn [ghcr.io/x/c:1]", entry, ok, digestOf('b'))
			}
		})
	}
}

// TestDependsOn_LocalDependencyConfined: a local dependsOn key is
// followed only from a local feature, only as a relative path, and only
// when it resolves (symlinks included) inside the directory of
// devcontainer.json; otherwise feature metadata could make the build
// copy and run any host directory.
func TestDependsOn_LocalDependencyConfined(t *testing.T) {
	t.Run("from a remote feature", func(t *testing.T) {
		f := newDepFixture(t, `{"ghcr.io/x/a:1":{}}`, map[string]depEntry{
			"ghcr.io/x/a": {digestOf('a'), meta("a", map[string]map[string]any{"./b": {}})},
		})
		writeLocalFeature(t, filepath.Join(f.ws, ".devcontainer", "b"), `{"id":"b","version":"1.0.0"}`)
		if _, err := f.layer(LockfileDisabled); err == nil || !strings.Contains(err.Error(), "only a local feature") {
			t.Fatalf("got %v, want a refusal of the local dependency", err)
		}
	})

	outside := t.TempDir()
	writeLocalFeature(t, filepath.Join(outside, "evil"), `{"id":"evil","version":"1.0.0"}`)
	for _, tc := range []struct {
		name, key, want string
		traverse, link  bool
	}{
		{name: "absolute path", key: filepath.Join(outside, "evil"), want: "relative path"},
		{name: "parent traversal", traverse: true, want: "outside"},
		{name: "symlink out", key: "./link", link: true, want: "outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := writeImageDevcontainer(t, `{"image":"alpine:3.20","features":{"./a":{}}}`)
			dc := filepath.Join(ws, ".devcontainer")
			key := tc.key
			if tc.traverse {
				// Reach the outside directory from .devcontainer with "..".
				rel, err := filepath.Rel(dc, filepath.Join(outside, "evil"))
				if err != nil {
					t.Fatal(err)
				}
				key = rel
			}
			if tc.link {
				if err := os.Symlink(filepath.Join(outside, "evil"), filepath.Join(dc, "link")); err != nil {
					t.Fatal(err)
				}
			}
			writeLocalFeature(t, filepath.Join(dc, "a"), `{"id":"a","version":"1.0.0","dependsOn":{"`+key+`":{}}}`)
			eng, err := New(EngineOptions{Runtime: newFakeRuntime()})
			if err != nil {
				t.Fatal(err)
			}
			f := &depFixture{t: t, eng: eng, ws: ws}
			if _, err := f.layer(LockfileDisabled); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("dependsOn %q: got %v, want an error containing %q", key, err, tc.want)
			}
		})
	}
}
