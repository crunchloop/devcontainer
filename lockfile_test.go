package devcontainer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/feature"
	"github.com/crunchloop/devcontainer/runtime"
)

const (
	lockDigestOld = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	lockDigestNew = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// tagStore is a feature.Store whose OCI tags currently point at
// lockDigestNew. It serves any repo@digest reference as that digest
// and records every reference it is asked for.
type tagStore struct {
	dir     string
	https   map[string]string // URL → content digest
	fetched []string
}

func (s *tagStore) Fetch(_ context.Context, ref string, kind config.FeatureSourceKind) (feature.Fetched, error) {
	s.fetched = append(s.fetched, ref)
	meta := config.FeatureMetadata{ID: "f", Version: "1.0.0"}
	switch kind {
	case config.FeatureSourceHTTPS:
		return feature.Fetched{Dir: s.dir, ResolvedRef: s.https[ref], Metadata: meta}, nil
	case config.FeatureSourceLocal:
		return feature.Fetched{Dir: s.dir, ResolvedRef: ref, Metadata: meta}, nil
	}
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return feature.Fetched{Dir: s.dir, ResolvedRef: ref, Metadata: meta}, nil
	}
	repo := ref[:strings.LastIndex(ref, ":")]
	return feature.Fetched{Dir: s.dir, ResolvedRef: repo + "@" + lockDigestNew, Metadata: meta}, nil
}

type lockFixture struct {
	t        *testing.T
	eng      *Engine
	rt       *fakeRuntime
	store    *tagStore
	ws       string
	lockPath string
}

func newLockFixture(t *testing.T, features string) *lockFixture {
	t.Helper()
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20","features":`+features+`}`)
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer", "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime()
	store := &tagStore{dir: t.TempDir(), https: map[string]string{}}
	eng, err := New(EngineOptions{Runtime: rt, FeatureStore: store})
	if err != nil {
		t.Fatal(err)
	}
	return &lockFixture{t: t, eng: eng, rt: rt, store: store, ws: ws,
		lockPath: filepath.Join(ws, ".devcontainer", "devcontainer-lock.json")}
}

// build runs Engine.Build. The fake runtime cannot build images, so a
// run that got past feature resolution ends in ErrNotImplemented; that
// error is swallowed and any other is returned.
func (f *lockFixture) build(mode LockfileMode) error {
	f.t.Helper()
	_, err := f.eng.Build(context.Background(), BuildOptions{LocalWorkspaceFolder: f.ws, Lockfile: mode})
	if errors.Is(err, runtime.ErrNotImplemented) {
		return nil
	}
	return err
}

func (f *lockFixture) writeLock(lf *feature.Lockfile) {
	f.t.Helper()
	if err := feature.WriteLockfile(f.lockPath, lf); err != nil {
		f.t.Fatal(err)
	}
}

func (f *lockFixture) readLock() *feature.Lockfile {
	f.t.Helper()
	lf, err := feature.ReadLockfile(f.lockPath)
	if err != nil {
		f.t.Fatal(err)
	}
	return lf
}

func ociEntry(repo, digest string) feature.LockedFeature {
	return feature.LockedFeature{Version: "1.0.0", Resolved: repo + "@" + digest, Integrity: digest}
}

func TestLockfile_DefaultWritesOCIAndHTTPSNotLocal(t *testing.T) {
	const url = "https://example.com/devcontainer-feature-h.tgz"
	f := newLockFixture(t, `{"ghcr.io/x/a:1":{},"`+url+`":{},"./local":{}}`)
	f.store.https[url] = lockDigestOld

	if err := f.build(LockfileDefault); err != nil {
		t.Fatal(err)
	}
	want := &feature.Lockfile{Features: map[string]feature.LockedFeature{
		"ghcr.io/x/a:1": ociEntry("ghcr.io/x/a", lockDigestNew),
		url:             {Version: "1.0.0", Resolved: url, Integrity: lockDigestOld},
	}}
	if got := f.readLock(); !got.Equal(want) {
		t.Errorf("lockfile differs from want in %v: %+v", got.Changed(want), got)
	}
}

func TestLockfile_PinsToLockedDigestAndLeavesFileAlone(t *testing.T) {
	f := newLockFixture(t, `{"ghcr.io/x/a:1":{}}`)
	// Hand-formatted: a rewrite would normalize it.
	body := `{"features":{"ghcr.io/x/a:1":{"version":"1.0.0","resolved":"ghcr.io/x/a@` + lockDigestOld + `","integrity":"` + lockDigestOld + `"}}}`
	if err := os.WriteFile(f.lockPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := f.build(LockfileDefault); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ghcr.io/x/a@" + lockDigestOld}; !reflect.DeepEqual(f.store.fetched, want) {
		t.Errorf("fetched %v, want %v", f.store.fetched, want)
	}
	if got, _ := os.ReadFile(f.lockPath); string(got) != body {
		t.Errorf("unchanged lockfile was rewritten:\n%s", got)
	}
}

func TestLockfile_DefaultDropsStaleAndAddsNewEntries(t *testing.T) {
	f := newLockFixture(t, `{"ghcr.io/x/a:1":{},"ghcr.io/x/b:1":{}}`)
	f.writeLock(&feature.Lockfile{Features: map[string]feature.LockedFeature{
		"ghcr.io/x/a:1":    ociEntry("ghcr.io/x/a", lockDigestOld),
		"ghcr.io/x/gone:1": ociEntry("ghcr.io/x/gone", lockDigestOld),
	}})

	if err := f.build(LockfileDefault); err != nil {
		t.Fatal(err)
	}
	want := &feature.Lockfile{Features: map[string]feature.LockedFeature{
		"ghcr.io/x/a:1": ociEntry("ghcr.io/x/a", lockDigestOld),
		"ghcr.io/x/b:1": ociEntry("ghcr.io/x/b", lockDigestNew),
	}}
	if got := f.readLock(); !got.Equal(want) {
		t.Errorf("lockfile differs from want in %v", got.Changed(want))
	}
}

func TestLockfile_UpgradeReresolvesAndRewrites(t *testing.T) {
	f := newLockFixture(t, `{"ghcr.io/x/a:1":{}}`)
	f.writeLock(&feature.Lockfile{Features: map[string]feature.LockedFeature{
		"ghcr.io/x/a:1": ociEntry("ghcr.io/x/a", lockDigestOld),
	}})

	if err := f.build(LockfileUpgrade); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ghcr.io/x/a:1"}; !reflect.DeepEqual(f.store.fetched, want) {
		t.Errorf("fetched %v, want %v", f.store.fetched, want)
	}
	if got, _ := f.readLock().Entry("ghcr.io/x/a:1"); got.Integrity != lockDigestNew {
		t.Errorf("integrity = %s, want %s", got.Integrity, lockDigestNew)
	}
}

func TestLockfile_FrozenMissingFailsBeforeFetching(t *testing.T) {
	f := newLockFixture(t, `{"ghcr.io/x/a:1":{}}`)

	err := f.build(LockfileFrozen)
	var me *feature.LockfileMismatchError
	if !errors.As(err, &me) || !me.Missing {
		t.Fatalf("got %v, want missing *LockfileMismatchError", err)
	}
	if len(f.store.fetched) != 0 {
		t.Errorf("fetched %v before failing", f.store.fetched)
	}
	if _, err := os.Stat(f.lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("frozen mode created the lockfile: %v", err)
	}
}

func TestLockfile_FrozenMismatchFailsWithoutWriting(t *testing.T) {
	f := newLockFixture(t, `{"ghcr.io/x/a:1":{},"ghcr.io/x/b:1":{}}`)
	f.writeLock(&feature.Lockfile{Features: map[string]feature.LockedFeature{
		"ghcr.io/x/a:1": ociEntry("ghcr.io/x/a", lockDigestOld),
	}})
	before, _ := os.ReadFile(f.lockPath)

	err := f.build(LockfileFrozen)
	var me *feature.LockfileMismatchError
	if !errors.As(err, &me) || me.Missing || !reflect.DeepEqual(me.Changed, []string{"ghcr.io/x/b:1"}) {
		t.Fatalf("got %v, want *LockfileMismatchError naming ghcr.io/x/b:1", err)
	}
	if after, _ := os.ReadFile(f.lockPath); string(after) != string(before) {
		t.Error("frozen mode rewrote the lockfile")
	}
}

func TestLockfile_FrozenMatchBuilds(t *testing.T) {
	f := newLockFixture(t, `{"ghcr.io/x/a:1":{}}`)
	f.writeLock(&feature.Lockfile{Features: map[string]feature.LockedFeature{
		"ghcr.io/x/a:1": ociEntry("ghcr.io/x/a", lockDigestOld),
	}})
	if err := f.build(LockfileFrozen); err != nil {
		t.Fatal(err)
	}
}

func TestLockfile_DisabledNeitherReadsNorWrites(t *testing.T) {
	f := newLockFixture(t, `{"ghcr.io/x/a:1":{}}`)
	if err := os.WriteFile(f.lockPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.build(LockfileDisabled); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(f.lockPath); string(got) != "{not json" {
		t.Errorf("disabled mode touched the lockfile: %s", got)
	}
}

func TestLockfile_HTTPSIntegrityMismatchFails(t *testing.T) {
	const url = "https://example.com/devcontainer-feature-h.tgz"
	f := newLockFixture(t, `{"`+url+`":{}}`)
	f.store.https[url] = lockDigestNew
	f.writeLock(&feature.Lockfile{Features: map[string]feature.LockedFeature{
		url: {Version: "1.0.0", Resolved: url, Integrity: lockDigestOld},
	}})

	var ie *feature.FeatureIntegrityError
	if err := f.build(LockfileDefault); !errors.As(err, &ie) {
		t.Fatalf("got %v, want *FeatureIntegrityError", err)
	}
}

// TestLockfile_KeepsEntryForFeatureBakedIntoBaseImage: a feature the
// base image's metadata label satisfies is not fetched, so its entry
// carries over rather than being dropped (which frozen mode would
// then reject on every run).
func TestLockfile_KeepsEntryForFeatureBakedIntoBaseImage(t *testing.T) {
	f := newLockFixture(t, `{"ghcr.io/x/a:1":{},"ghcr.io/x/b:1":{}}`)
	f.rt.imagesByRef["alpine:3.20"] = &runtime.ImageDetails{Labels: map[string]string{
		feature.MetadataLabel: `[{"id":"a","version":"1.0.0"}]`,
	}}
	locked := &feature.Lockfile{Features: map[string]feature.LockedFeature{
		"ghcr.io/x/a:1": ociEntry("ghcr.io/x/a", lockDigestOld),
		"ghcr.io/x/b:1": ociEntry("ghcr.io/x/b", lockDigestOld),
	}}
	f.writeLock(locked)

	if err := f.build(LockfileFrozen); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ghcr.io/x/b@" + lockDigestOld}; !reflect.DeepEqual(f.store.fetched, want) {
		t.Errorf("fetched %v, want only b", f.store.fetched)
	}
}

// TestLockfile_DotDevcontainerJSON: with a discovered root
// .devcontainer.json, the lockfile is .devcontainer-lock.json beside it
// and local features resolve relative to the workspace root.
func TestLockfile_DotDevcontainerJSON(t *testing.T) {
	ws := t.TempDir()
	body := `{"image":"alpine:3.20","features":{"ghcr.io/x/a:1":{},"./local":{}}}`
	if err := os.WriteFile(filepath.Join(ws, ".devcontainer.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &tagStore{dir: t.TempDir()}
	eng, err := New(EngineOptions{Runtime: newFakeRuntime(), FeatureStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Build(context.Background(), BuildOptions{LocalWorkspaceFolder: ws}); !errors.Is(err, runtime.ErrNotImplemented) {
		t.Fatalf("Build: %v", err)
	}
	lf, err := feature.ReadLockfile(filepath.Join(ws, ".devcontainer-lock.json"))
	if err != nil || lf == nil {
		t.Fatalf("no .devcontainer-lock.json: (%v, %v)", lf, err)
	}
	if want := filepath.Join(ws, "local"); !slices.Contains(store.fetched, want) {
		t.Errorf("fetched %v, want local feature at %s", store.fetched, want)
	}
}
