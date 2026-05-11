package devcontainer

import (
	"context"
	"fmt"
	"os"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/feature"
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
//
// The returned Workspace.Config is the MINIMAL form (LocalWorkspaceFolder,
// ContainerWorkspaceFolder, ContainerUser/RemoteUser, source kind plus
// any image-metadata-merged fields). Devcontainer.json-only fields
// (Lifecycle hooks, Mounts, Customizations, Features) are NOT
// reconstructed here — Attach does not re-read the source
// devcontainer.json. Callers that need the full ResolvedConfig should
// either call Resolve directly or use Engine.Up. See the Workspace
// type docs for the full breakdown.
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

	// The container's image carries the merged-config metadata label
	// from when Up created it; folding it in here means Attach-only
	// callers see the same RemoteUser / lifecycle hooks / probe config
	// as Up. Failures to read or parse the label are non-fatal — Attach
	// then gives back a minimal cfg as before.
	var baseLayers []config.FeatureMetadata
	if details.Image != "" {
		if imgDetails, err := e.runtime.InspectImage(ctx, details.Image); err == nil && imgDetails != nil {
			if label := imgDetails.Labels[feature.MetadataLabel]; label != "" {
				if parsed, err := feature.ParseMetadataLabel(label); err == nil {
					baseLayers = parsed
				}
			}
		}
	}
	config.MergeMetadata(cfg, baseLayers)
	cfg.Finalize()

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
