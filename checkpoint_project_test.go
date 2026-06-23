package devcontainer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/crunchloop/devcontainer/compose"
	"github.com/crunchloop/devcontainer/runtime"
)

// fakeProjectRuntime is a CheckpointRuntime that round-trips a whole
// project: ListContainers filters the seeded set by label, Checkpoint
// records each container's labels keyed by archive path (and writes a stub
// archive file), and Restore recreates a fresh container from those
// recorded labels — modelling podman preserving labels across the archive.
type fakeProjectRuntime struct {
	*fakeRuntime
	archiveLabels map[string]map[string]string // archive path → original labels
	restoreSeq    int

	// failCheckpointID makes Checkpoint fail for that container id (to
	// exercise the partial-failure → no-manifest path).
	failCheckpointID string
	// restoreErr, when set, makes every Restore fail with it.
	restoreErr error
}

func newFakeProjectRuntime() *fakeProjectRuntime {
	return &fakeProjectRuntime{fakeRuntime: newFakeRuntime(), archiveLabels: map[string]map[string]string{}}
}

func (f *fakeProjectRuntime) Capabilities() runtime.Capabilities {
	c := f.fakeRuntime.Capabilities()
	c.Checkpoint = true
	return c
}

// CreateNetwork succeeds: RestoreProject recreates the project network
// before restoring containers (cross-node fresh-store path), so the
// checkpoint-capable fake must support it.
func (f *fakeProjectRuntime) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) (string, error) {
	return "net-" + spec.Name, nil
}

func (f *fakeProjectRuntime) ListContainers(ctx context.Context, filter runtime.LabelFilter) ([]runtime.Container, error) {
	f.fakeRuntime.mu.Lock()
	defer f.fakeRuntime.mu.Unlock()
	var out []runtime.Container
	for _, d := range f.fakeRuntime.containersByID {
		if labelsMatch(d.Labels, filter.Match) {
			c := d.Container
			c.Labels = d.Labels
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeProjectRuntime) Checkpoint(ctx context.Context, id string, spec runtime.CheckpointSpec) (runtime.CheckpointRef, error) {
	if id == f.failCheckpointID {
		return runtime.CheckpointRef{}, &runtime.CheckpointFailedError{ID: id, Err: errors.New("injected checkpoint failure")}
	}
	f.fakeRuntime.mu.Lock()
	var labels map[string]string
	if d := f.fakeRuntime.containersByID[id]; d != nil {
		labels = d.Labels
	}
	f.archiveLabels[spec.ArchivePath] = labels
	f.fakeRuntime.mu.Unlock()
	if err := os.WriteFile(spec.ArchivePath, []byte("FAKE-TAR"), 0o600); err != nil {
		return runtime.CheckpointRef{}, err
	}
	return runtime.CheckpointRef{ArchivePath: spec.ArchivePath, Size: int64(len("FAKE-TAR"))}, nil
}

func (f *fakeProjectRuntime) Restore(ctx context.Context, spec runtime.RestoreSpec) (*runtime.Container, error) {
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	f.fakeRuntime.mu.Lock()
	defer f.fakeRuntime.mu.Unlock()
	f.restoreSeq++
	id := fmt.Sprintf("restored-%d", f.restoreSeq)
	labels := f.archiveLabels[spec.ArchivePath]
	c := &runtime.Container{ID: id, State: runtime.StateRunning, Labels: labels}
	f.fakeRuntime.containersByID[id] = &runtime.ContainerDetails{
		Container: *c,
		Labels:    labels,
		Env:       []string{"HOME=/root", "PATH=/usr/bin"},
	}
	return c, nil
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func seedProjectContainer(f *fakeProjectRuntime, id string, labels map[string]string) {
	f.fakeRuntime.mu.Lock()
	defer f.fakeRuntime.mu.Unlock()
	f.fakeRuntime.containersByID[id] = &runtime.ContainerDetails{
		Container: runtime.Container{ID: id, Name: id, State: runtime.StateRunning, Labels: labels},
		Labels:    labels,
	}
}

func TestCheckpointRestoreProject_RoundTrip(t *testing.T) {
	rt := newFakeProjectRuntime()
	// A 3-service project: primary "app" carries the devcontainer id;
	// "db" and "cache" are plain sidecars (compose labels only).
	seedProjectContainer(rt, "app-1", map[string]string{
		compose.LabelComposeProject: "dc-proj",
		compose.LabelComposeService: "app",
		LabelDevcontainerID:         "ws-app",
		LabelLocalWorkspaceFolder:   "/work",
	})
	seedProjectContainer(rt, "db-1", map[string]string{
		compose.LabelComposeProject: "dc-proj",
		compose.LabelComposeService: "db",
	})
	seedProjectContainer(rt, "cache-1", map[string]string{
		compose.LabelComposeProject: "dc-proj",
		compose.LabelComposeService: "cache",
	})
	// A container from a DIFFERENT project must not be swept in.
	seedProjectContainer(rt, "other-1", map[string]string{
		compose.LabelComposeProject: "other-proj",
		compose.LabelComposeService: "app",
	})

	eng, _ := New(EngineOptions{Runtime: rt})
	ws := &Workspace{Container: &runtime.ContainerDetails{
		Container: runtime.Container{ID: "app-1"},
		Labels:    map[string]string{compose.LabelComposeProject: "dc-proj"},
	}}

	ctx := context.Background()
	dir := t.TempDir()

	ref, err := eng.CheckpointProject(ctx, ws, ProjectCheckpointOptions{ArchiveDir: dir, TCPEstablished: true})
	if err != nil {
		t.Fatalf("CheckpointProject: %v", err)
	}
	if ref.Project != "dc-proj" || len(ref.Services) != 3 {
		t.Fatalf("ref = %+v (want project dc-proj, 3 services)", ref)
	}
	// Deterministic service-name order: app, cache, db.
	if ref.Services[0].Service != "app" || ref.Services[1].Service != "cache" || ref.Services[2].Service != "db" {
		t.Fatalf("service order = %q/%q/%q, want app/cache/db", ref.Services[0].Service, ref.Services[1].Service, ref.Services[2].Service)
	}
	// Manifest written; archive files present.
	if _, err := os.Stat(filepath.Join(dir, projectManifestName)); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	for _, s := range ref.Services {
		if _, err := os.Stat(filepath.Join(dir, s.Archive)); err != nil {
			t.Fatalf("archive for %q missing: %v", s.Service, err)
		}
	}

	pr, err := eng.RestoreProject(ctx, ProjectRestoreOptions{ArchiveDir: dir, TCPEstablished: true})
	if err != nil {
		t.Fatalf("RestoreProject: %v", err)
	}
	if pr.Project != "dc-proj" || len(pr.Services) != 3 {
		t.Fatalf("restore = %+v (want project dc-proj, 3 services)", pr)
	}
	// Every service came back.
	for _, svc := range []string{"app", "cache", "db"} {
		if pr.Services[svc] == nil {
			t.Errorf("service %q not restored", svc)
		}
	}
	// The devcontainer service is reattached as the Primary workspace, id
	// recovered from the preserved label.
	if pr.Primary == nil {
		t.Fatal("Primary workspace is nil — devcontainer service not reattached")
	}
	if pr.Primary.ID != "ws-app" {
		t.Errorf("Primary.ID = %q, want ws-app (from dev.containers.id label)", pr.Primary.ID)
	}
	if pr.Primary.subst == nil {
		t.Error("Primary workspace has no substituter")
	}
}

func TestCheckpointProject_Validation(t *testing.T) {
	rt := newFakeProjectRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ctx := context.Background()

	// Not a compose workspace (no project label).
	ws := &Workspace{Container: &runtime.ContainerDetails{Container: runtime.Container{ID: "c1"}}}
	if _, err := eng.CheckpointProject(ctx, ws, ProjectCheckpointOptions{ArchiveDir: t.TempDir()}); err == nil {
		t.Fatal("want error for non-compose workspace")
	}
	// Missing ArchiveDir.
	composeWS := &Workspace{Container: &runtime.ContainerDetails{
		Labels: map[string]string{compose.LabelComposeProject: "p"},
	}}
	if _, err := eng.CheckpointProject(ctx, composeWS, ProjectCheckpointOptions{}); err == nil {
		t.Fatal("want error for empty ArchiveDir")
	}
	// RestoreProject with no manifest in the dir.
	if _, err := eng.RestoreProject(ctx, ProjectRestoreOptions{ArchiveDir: t.TempDir()}); err == nil {
		t.Fatal("want error for missing manifest")
	}
}

// A mid-loop service failure aborts CheckpointProject WITHOUT writing the
// manifest, so a later RestoreProject fails cleanly rather than restoring a
// partial set.
func TestCheckpointProject_PartialFailureWritesNoManifest(t *testing.T) {
	rt := newFakeProjectRuntime()
	rt.failCheckpointID = "db-1" // "db" sorts after "app" → app succeeds, db fails
	seedProjectContainer(rt, "app-1", map[string]string{
		compose.LabelComposeProject: "p", compose.LabelComposeService: "app", LabelDevcontainerID: "ws",
	})
	seedProjectContainer(rt, "db-1", map[string]string{
		compose.LabelComposeProject: "p", compose.LabelComposeService: "db",
	})
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := &Workspace{Container: &runtime.ContainerDetails{
		Labels: map[string]string{compose.LabelComposeProject: "p"},
	}}
	dir := t.TempDir()

	if _, err := eng.CheckpointProject(context.Background(), ws, ProjectCheckpointOptions{ArchiveDir: dir}); err == nil {
		t.Fatal("want error when a service checkpoint fails")
	}
	if _, err := os.Stat(filepath.Join(dir, projectManifestName)); !os.IsNotExist(err) {
		t.Fatalf("manifest must be absent after a partial failure (stat err = %v)", err)
	}
}

// A per-service restore failure propagates (wrapped) as *RestoreFailedError
// so callers can fall back to a cold project Up.
func TestRestoreProject_BackendErrorPropagates(t *testing.T) {
	rt := newFakeProjectRuntime()
	seedProjectContainer(rt, "app-1", map[string]string{
		compose.LabelComposeProject: "p", compose.LabelComposeService: "app", LabelDevcontainerID: "ws",
	})
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := &Workspace{Container: &runtime.ContainerDetails{
		Labels: map[string]string{compose.LabelComposeProject: "p"},
	}}
	dir := t.TempDir()
	if _, err := eng.CheckpointProject(context.Background(), ws, ProjectCheckpointOptions{ArchiveDir: dir}); err != nil {
		t.Fatalf("CheckpointProject setup: %v", err)
	}

	rt.restoreErr = &runtime.RestoreFailedError{ArchivePath: "x", Err: errors.New("criu boom")}
	_, err := eng.RestoreProject(context.Background(), ProjectRestoreOptions{ArchiveDir: dir})
	var rfe *runtime.RestoreFailedError
	if !errors.As(err, &rfe) {
		t.Fatalf("want *RestoreFailedError, got %v", err)
	}
}

func TestCheckpointProject_UnsupportedBackend(t *testing.T) {
	eng, _ := New(EngineOptions{Runtime: newFakeRuntime()})
	ws := &Workspace{Container: &runtime.ContainerDetails{
		Labels: map[string]string{compose.LabelComposeProject: "p"},
	}}
	_, err := eng.CheckpointProject(context.Background(), ws, ProjectCheckpointOptions{ArchiveDir: t.TempDir()})
	if err == nil {
		t.Fatal("want ErrCheckpointUnsupported for a non-checkpoint backend")
	}
}
