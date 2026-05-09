package devcontainer

import (
	"context"
	"fmt"
	"os"

	"github.com/crunchloop/devcontainer/runtime"
)

// AttachOptions configures Engine.Attach. The default zero value is fine
// for most callers — it discovers the container by label and reads the
// host process environment for any localEnv references the substituter
// might still need.
type AttachOptions struct {
	// LocalEnv overrides os.Environ() for the substituter's localEnv
	// pass. Nil means use the current process environment.
	LocalEnv map[string]string
}

// Attach finds an existing workspace container by its devcontainer id and
// returns a *Workspace with a substituter bound to its live env.
//
// Returns *runtime.ContainerNotFoundError if no container with the
// matching label exists. The returned workspace's Config.LocalEnv is the
// AttachOptions.LocalEnv (or os.Environ() if nil) — note that
// LocalWorkspaceFolder and ConfigPath cannot be recovered from a running
// container alone, so callers needing those should use Up.
func (e *Engine) Attach(ctx context.Context, id WorkspaceID) (*Workspace, error) {
	return e.AttachWith(ctx, id, AttachOptions{})
}

// AttachWith is Attach plus options.
func (e *Engine) AttachWith(ctx context.Context, id WorkspaceID, opts AttachOptions) (*Workspace, error) {
	if err := ctxIfDone(ctx); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("Engine.Attach: WorkspaceID is required")
	}

	c, err := e.runtime.FindContainerByLabel(ctx, LabelDevcontainerID, string(id))
	if err != nil {
		return nil, fmt.Errorf("find container for workspace %s: %w", id, err)
	}
	if c == nil {
		return nil, &runtime.ContainerNotFoundError{ID: string(id)}
	}

	details, err := e.runtime.InspectContainer(ctx, c.ID)
	if err != nil {
		return nil, err
	}

	// Reconstruct just enough config for the substituter. Attach can't
	// reproduce the full ResolvedConfig (the source devcontainer.json may
	// have changed since Up); callers that need it should Resolve again.
	cfg := configFromContainerLabels(details)
	cfg.DevcontainerID = string(id)

	localEnv := opts.LocalEnv
	if localEnv == nil {
		localEnv = environAsMap(os.Environ())
	}

	ws := &Workspace{
		ID:        id,
		Config:    cfg,
		Container: details,
		subst:     newSubstituter(cfg, details, localEnv),
	}

	// Re-probe on attach so subsequent Exec calls see PATH additions
	// from the user's rc files. The original Up populated probedEnv,
	// but a fresh Attach doesn't share that workspace value.
	if probed, err := e.probeUserEnv(ctx, ws, cfg.UserEnvProbe); err == nil {
		ws.probedEnv = probed
	}
	return ws, nil
}
