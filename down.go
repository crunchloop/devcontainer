package devcontainer

import (
	"context"
	"errors"
	"fmt"

	"github.com/crunchloop/devcontainer/runtime"
)

// DownOptions configures Engine.Down.
type DownOptions struct {
	// Remove, when true, removes the container after stopping it. Default
	// false leaves the container in stopped state for fast subsequent Up.
	Remove bool

	// RemoveVolumes, when true with Remove, also removes anonymous volumes
	// the container created. Has no effect when Remove is false.
	RemoveVolumes bool
}

// Down stops the workspace's container. With opts.Remove, the container
// is also removed (matching shutdownAction=stopContainer style cleanup).
//
// Down is safe to call on a workspace whose container has already been
// stopped or removed externally — the underlying ContainerNotFoundError
// is treated as success.
func (e *Engine) Down(ctx context.Context, ws *Workspace, opts DownOptions) error {
	if err := ctxIfDone(ctx); err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("Engine.Down: Workspace is required")
	}
	id := ws.Container.ID

	if err := e.runtime.StopContainer(ctx, id, runtime.StopOptions{}); err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("stop container %s: %w", id, err)
		}
	}

	if opts.Remove {
		if err := e.runtime.RemoveContainer(ctx, id, runtime.RemoveOptions{
			RemoveVolumes: opts.RemoveVolumes,
		}); err != nil && !isNotFound(err) {
			return fmt.Errorf("remove container %s: %w", id, err)
		}
	}
	return nil
}

func isNotFound(err error) bool {
	var nf *runtime.ContainerNotFoundError
	return errors.As(err, &nf)
}
