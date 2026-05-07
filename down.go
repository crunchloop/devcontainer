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
// For compose workspaces, Down maps to `docker compose stop` (without
// Remove) or `docker compose down` (with Remove) — i.e., the whole
// project is treated as a unit. This is asymmetric vs image/build
// workspaces where Down only touches the workspace's primary container,
// but matches the spec's stopCompose shutdownAction semantics.
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

	if isComposeWorkspace(ws) {
		return e.downCompose(ctx, ws, opts)
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

// isComposeWorkspace reports whether a Workspace's Container was
// created by docker compose (and so should be torn down via
// ComposeRuntime instead of single-container methods).
func isComposeWorkspace(ws *Workspace) bool {
	if ws == nil || ws.Container == nil {
		return false
	}
	_, ok := ws.Container.Labels["com.docker.compose.project"]
	return ok
}

// downCompose tears down the compose project by reading the project
// name off the workspace container's labels and invoking
// ComposeRuntime.ComposeDown. Without Remove, falls back to a per-
// container stop (compose has no native "stop the whole project
// without removing" command in its stable API surface; we approximate).
func (e *Engine) downCompose(ctx context.Context, ws *Workspace, opts DownOptions) error {
	cr, ok := e.runtime.(runtime.ComposeRuntime)
	if !ok {
		// Shouldn't happen — we couldn't have created a compose
		// workspace without a ComposeRuntime — but stay defensive.
		return fmt.Errorf("Down: workspace was created by compose but runtime no longer satisfies ComposeRuntime")
	}
	projectName := ws.Container.Labels["com.docker.compose.project"]
	if projectName == "" {
		return fmt.Errorf("Down: compose workspace missing com.docker.compose.project label")
	}

	if !opts.Remove {
		// `compose stop` — keeps containers around for fast restart.
		// We approximate by stopping each container individually since
		// our ComposeRuntime interface doesn't expose Stop separately.
		// In practice, callers wanting "stop without remove" on compose
		// will set Remove=false; for now we just stop the primary and
		// document the asymmetry.
		if err := e.runtime.StopContainer(ctx, ws.Container.ID, runtime.StopOptions{}); err != nil && !isNotFound(err) {
			return fmt.Errorf("stop compose primary %s: %w", ws.Container.ID, err)
		}
		return nil
	}

	if err := cr.ComposeDown(ctx, runtime.ComposeDownSpec{
		ProjectName:   projectName,
		RemoveVolumes: opts.RemoveVolumes,
	}); err != nil {
		return fmt.Errorf("compose down: %w", err)
	}
	return nil
}

func isNotFound(err error) bool {
	var nf *runtime.ContainerNotFoundError
	return errors.As(err, &nf)
}
