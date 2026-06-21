package devcontainer

import (
	"context"
	"errors"
	"testing"

	"github.com/crunchloop/devcontainer/feature"
	"github.com/crunchloop/devcontainer/runtime"
)

// fakeCheckpointRuntime wraps fakeRuntime to also implement
// runtime.CheckpointRuntime and advertise Capabilities().Checkpoint.
type fakeCheckpointRuntime struct {
	*fakeRuntime
	checkpointable bool

	gotCheckpointID   string
	gotCheckpointSpec runtime.CheckpointSpec
	gotRestoreSpec    runtime.RestoreSpec
	checkpointErr     error
	restoreErr        error
}

func (f *fakeCheckpointRuntime) Capabilities() runtime.Capabilities {
	c := f.fakeRuntime.Capabilities()
	c.Checkpoint = f.checkpointable
	return c
}

func (f *fakeCheckpointRuntime) Checkpoint(ctx context.Context, id string, spec runtime.CheckpointSpec) (runtime.CheckpointRef, error) {
	f.gotCheckpointID = id
	f.gotCheckpointSpec = spec
	if f.checkpointErr != nil {
		return runtime.CheckpointRef{}, f.checkpointErr
	}
	return runtime.CheckpointRef{ArchivePath: spec.ArchivePath, Size: 42}, nil
}

func (f *fakeCheckpointRuntime) Restore(ctx context.Context, spec runtime.RestoreSpec) (*runtime.Container, error) {
	f.gotRestoreSpec = spec
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	c := &runtime.Container{ID: "restored-1", State: runtime.StateRunning}
	// Register the restored container so Engine.Restore's reattach
	// (inspect → rebuild Workspace) finds it. Real podman restore
	// preserves the original's labels in the new container, so carry the
	// devcontainer id + local-workspace labels the reattach reads.
	f.fakeRuntime.mu.Lock()
	f.fakeRuntime.containersByID["restored-1"] = &runtime.ContainerDetails{
		Container: *c,
		Labels: map[string]string{
			LabelDevcontainerID:       "ws-restored-id",
			LabelLocalWorkspaceFolder: "/work",
		},
		Env: []string{"HOME=/root", "PATH=/usr/bin"},
	}
	f.fakeRuntime.mu.Unlock()
	return c, nil
}

func wsWithContainer(id string) *Workspace {
	return &Workspace{Container: &runtime.ContainerDetails{Container: runtime.Container{ID: id}}}
}

// A runtime that doesn't implement CheckpointRuntime at all (plain
// fakeRuntime) must surface ErrCheckpointUnsupported on both verbs.
func TestCheckpoint_UnsupportedBackend(t *testing.T) {
	eng, _ := New(EngineOptions{Runtime: newFakeRuntime()})

	_, err := eng.Checkpoint(context.Background(), wsWithContainer("c1"), CheckpointOptions{ArchivePath: "/tmp/a.tar"})
	if !errors.Is(err, runtime.ErrCheckpointUnsupported) {
		t.Fatalf("Checkpoint: want ErrCheckpointUnsupported, got %v", err)
	}

	_, err = eng.Restore(context.Background(), RestoreOptions{ArchivePath: "/tmp/a.tar"})
	if !errors.Is(err, runtime.ErrCheckpointUnsupported) {
		t.Fatalf("Restore: want ErrCheckpointUnsupported, got %v", err)
	}
}

// A backend that implements the interface but advertises
// Capabilities().Checkpoint == false is still unsupported (e.g. podman
// present but criu check failed).
func TestCheckpoint_CapabilityFalseIsUnsupported(t *testing.T) {
	rt := &fakeCheckpointRuntime{fakeRuntime: newFakeRuntime(), checkpointable: false}
	eng, _ := New(EngineOptions{Runtime: rt})

	_, err := eng.Checkpoint(context.Background(), wsWithContainer("c1"), CheckpointOptions{ArchivePath: "/tmp/a.tar"})
	if !errors.Is(err, runtime.ErrCheckpointUnsupported) {
		t.Fatalf("Checkpoint: want ErrCheckpointUnsupported, got %v", err)
	}
	if rt.gotCheckpointID != "" {
		t.Fatalf("backend Checkpoint should not be called when capability is false")
	}
}

func TestCheckpoint_HappyPath(t *testing.T) {
	rt := &fakeCheckpointRuntime{fakeRuntime: newFakeRuntime(), checkpointable: true}
	eng, _ := New(EngineOptions{Runtime: rt})

	ref, err := eng.Checkpoint(context.Background(), wsWithContainer("c1"), CheckpointOptions{
		ArchivePath:    "/vol/ckpt.tar",
		StopAfter:      true,
		TCPEstablished: true,
	})
	if err != nil {
		t.Fatalf("Checkpoint: unexpected error: %v", err)
	}
	if ref.ArchivePath != "/vol/ckpt.tar" || ref.Size != 42 {
		t.Fatalf("Checkpoint: unexpected ref %+v", ref)
	}
	// Spec is threaded through from options + the workspace container id.
	if rt.gotCheckpointID != "c1" {
		t.Fatalf("Checkpoint: backend got id %q, want c1", rt.gotCheckpointID)
	}
	if rt.gotCheckpointSpec.ArchivePath != "/vol/ckpt.tar" || !rt.gotCheckpointSpec.StopAfter || !rt.gotCheckpointSpec.TCPEstablished {
		t.Fatalf("Checkpoint: backend got spec %+v", rt.gotCheckpointSpec)
	}
}

func TestRestore_HappyPath(t *testing.T) {
	rt := &fakeCheckpointRuntime{fakeRuntime: newFakeRuntime(), checkpointable: true}
	eng, _ := New(EngineOptions{Runtime: rt})

	ws, err := eng.Restore(context.Background(), RestoreOptions{ArchivePath: "/vol/ckpt.tar", Name: "ws-restored", TCPEstablished: true})
	if err != nil {
		t.Fatalf("Restore: unexpected error: %v", err)
	}
	// Restore reattaches a full *Workspace around the restored container:
	// the container handle, the workspace id recovered from its label, and
	// a substituter bound to its live env.
	if ws == nil || ws.Container == nil || ws.Container.ID != "restored-1" {
		t.Fatalf("Restore: unexpected workspace %+v", ws)
	}
	if ws.ID != "ws-restored-id" {
		t.Fatalf("Restore: workspace id = %q, want ws-restored-id (from container label)", ws.ID)
	}
	if ws.subst == nil {
		t.Fatal("Restore: workspace has no substituter")
	}
	if rt.gotRestoreSpec.ArchivePath != "/vol/ckpt.tar" || rt.gotRestoreSpec.Name != "ws-restored" || !rt.gotRestoreSpec.TCPEstablished {
		t.Fatalf("Restore: backend got spec %+v", rt.gotRestoreSpec)
	}
}

// A backend RestoreFailedError propagates (wrapped) so callers can
// distinguish it from a cold-start failure and fall back to a cold Up.
func TestRestore_BackendErrorPropagates(t *testing.T) {
	want := &runtime.RestoreFailedError{ArchivePath: "/vol/ckpt.tar", Err: errors.New("criu boom")}
	rt := &fakeCheckpointRuntime{fakeRuntime: newFakeRuntime(), checkpointable: true, restoreErr: want}
	eng, _ := New(EngineOptions{Runtime: rt})

	_, err := eng.Restore(context.Background(), RestoreOptions{ArchivePath: "/vol/ckpt.tar"})
	var rfe *runtime.RestoreFailedError
	if !errors.As(err, &rfe) {
		t.Fatalf("Restore: want *RestoreFailedError, got %v", err)
	}
}

// reattachWorkspace (shared by Attach and Restore/RestoreProject) folds the
// restored image's devcontainer.metadata label into the reconstructed
// config — so a reattached workspace sees the same RemoteUser etc. as Up.
func TestReattachWorkspace_MergesImageMetadataLabel(t *testing.T) {
	rt := newFakeRuntime()
	rt.imagesByRef["img-meta"] = &runtime.ImageDetails{
		Labels: map[string]string{feature.MetadataLabel: `[{"remoteUser":"dc-user"}]`},
	}
	eng, _ := New(EngineOptions{Runtime: rt})

	details := &runtime.ContainerDetails{
		Container: runtime.Container{ID: "rc", Image: "img-meta", State: runtime.StateRunning},
		Labels:    map[string]string{LabelDevcontainerID: "ws"},
		Env:       []string{"HOME=/root"},
	}
	ws := eng.reattachWorkspace(context.Background(), details, "ws", nil)
	if ws.Config.RemoteUser != "dc-user" {
		t.Fatalf("RemoteUser = %q, want dc-user (merged from the image %s label)", ws.Config.RemoteUser, feature.MetadataLabel)
	}
}

func TestCheckpoint_Validation(t *testing.T) {
	rt := &fakeCheckpointRuntime{fakeRuntime: newFakeRuntime(), checkpointable: true}
	eng, _ := New(EngineOptions{Runtime: rt})

	// No container on the workspace.
	if _, err := eng.Checkpoint(context.Background(), &Workspace{}, CheckpointOptions{ArchivePath: "/tmp/a.tar"}); err == nil {
		t.Fatal("Checkpoint: want error for workspace with no container")
	}
	// Missing archive path.
	if _, err := eng.Checkpoint(context.Background(), wsWithContainer("c1"), CheckpointOptions{}); err == nil {
		t.Fatal("Checkpoint: want error for empty ArchivePath")
	}
	if _, err := eng.Restore(context.Background(), RestoreOptions{}); err == nil {
		t.Fatal("Restore: want error for empty ArchivePath")
	}
}
