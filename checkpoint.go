package devcontainer

import (
	"context"
	"fmt"

	"github.com/crunchloop/devcontainer/runtime"
)

// CheckpointOptions configures Engine.Checkpoint.
type CheckpointOptions struct {
	// ArchivePath is where the portable checkpoint archive is written.
	// Required. Point it at durable, transferable storage (the workspace
	// volume, object storage) — the archive is self-contained, so a
	// later Restore can run on a different node by moving this file.
	ArchivePath string

	// StopAfter stops/removes the container once the archive is written
	// — the spot-eviction path, where the node is going away anyway.
	// False keeps the container running ("backup" checkpoint).
	StopAfter bool

	// TCPEstablished requests checkpoint of established TCP connections.
	// Recommended true for devcontainers: a container holding a live
	// connection at checkpoint time fails to checkpoint without it.
	TCPEstablished bool
}

// RestoreOptions configures Engine.Restore.
type RestoreOptions struct {
	// ArchivePath is the archive a prior Checkpoint wrote. Required.
	ArchivePath string

	// Name optionally names the restored container.
	Name string

	// TCPEstablished must match the checkpoint when the archive captured
	// established connections.
	TCPEstablished bool

	// LocalEnv overrides os.Environ() for the reattached workspace's
	// substituter localEnv pass. Nil means use the current process
	// environment — matches AttachOptions.LocalEnv. On a cross-node
	// restore the destination's env may differ from the source's, so a
	// caller that cares can pin it here.
	LocalEnv map[string]string
}

// Checkpoint writes a portable checkpoint archive for the workspace's
// container (process + memory state plus the writable rootfs), so it can
// later be restored — possibly on another node — by Restore.
//
// Returns ErrCheckpointUnsupported (wrapped) if the active backend does
// not implement runtime.CheckpointRuntime or advertises
// Capabilities().Checkpoint == false. Callers can errors.Is against
// runtime.ErrCheckpointUnsupported and fall back to a cold path.
//
// Checkpoint is the primitive; deciding *when* to checkpoint (e.g. on a
// spot-reclaim notice) is the caller's job.
func (e *Engine) Checkpoint(ctx context.Context, ws *Workspace, opts CheckpointOptions) (runtime.CheckpointRef, error) {
	if err := ctxIfDone(ctx); err != nil {
		return runtime.CheckpointRef{}, err
	}
	if ws == nil || ws.Container == nil {
		return runtime.CheckpointRef{}, fmt.Errorf("Checkpoint: workspace has no container")
	}
	if opts.ArchivePath == "" {
		return runtime.CheckpointRef{}, fmt.Errorf("Checkpoint: ArchivePath is required")
	}

	cr, ok := e.runtime.(runtime.CheckpointRuntime)
	if !ok || !e.runtime.Capabilities().Checkpoint {
		return runtime.CheckpointRef{}, fmt.Errorf("Checkpoint: %w", runtime.ErrCheckpointUnsupported)
	}

	ref, err := cr.Checkpoint(ctx, ws.Container.ID, runtime.CheckpointSpec{
		ArchivePath:    opts.ArchivePath,
		StopAfter:      opts.StopAfter,
		TCPEstablished: opts.TCPEstablished,
	})
	if err != nil {
		return runtime.CheckpointRef{}, fmt.Errorf("checkpoint: %w", err)
	}
	return ref, nil
}

// Restore re-creates and resumes a container from a checkpoint archive
// written by Checkpoint, reconstructing its mounts and re-attaching
// networking, then rebuilds the *Workspace around it. The original
// container may be gone (the migration case).
//
// The returned Workspace has the MINIMAL config Attach produces — the
// devcontainer labels the checkpoint archive preserves plus the image's
// merged-config metadata — with the substituter bound to the restored
// container's live env and userEnv re-probed. It is enough to drive Exec
// and Down; callers needing the full devcontainer.json view should
// Resolve from source. See the Workspace type docs.
//
// Returns ErrCheckpointUnsupported (wrapped) when the backend can't, and
// a *runtime.RestoreFailedError (from the backend) on a restore failure
// — distinct from a cold-start failure, so callers can fall back to a
// cold Up on the (intact) workspace volume.
func (e *Engine) Restore(ctx context.Context, opts RestoreOptions) (*Workspace, error) {
	if err := ctxIfDone(ctx); err != nil {
		return nil, err
	}
	if opts.ArchivePath == "" {
		return nil, fmt.Errorf("Restore: ArchivePath is required")
	}

	cr, ok := e.runtime.(runtime.CheckpointRuntime)
	if !ok || !e.runtime.Capabilities().Checkpoint {
		return nil, fmt.Errorf("Restore: %w", runtime.ErrCheckpointUnsupported)
	}

	c, err := cr.Restore(ctx, runtime.RestoreSpec{
		ArchivePath:    opts.ArchivePath,
		Name:           opts.Name,
		TCPEstablished: opts.TCPEstablished,
	})
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}

	// Reattach: the restored container carries the devcontainer labels
	// from the archive, so rebuild the Workspace the same way Attach
	// does. inspectStable absorbs the post-restore state lag (the daemon
	// reports state asynchronously after import-and-start). The workspace
	// id is recovered from the container's label.
	details, err := e.inspectStable(ctx, c.ID)
	if err != nil {
		return nil, fmt.Errorf("restore: inspect restored container %s: %w", c.ID, err)
	}
	id := WorkspaceID(details.Labels[LabelDevcontainerID])
	return e.reattachWorkspace(ctx, details, id, opts.LocalEnv), nil
}
