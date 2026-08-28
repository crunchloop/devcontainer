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

	return e.reattachWorkspace(ctx, details, id, opts.LocalEnv), nil
}

// reattachWorkspace rebuilds a *Workspace from an already-inspected,
// running container found by label. It needs only the MINIMAL config +
// bound substituter + userEnv probe, without re-reading
// devcontainer.json.
//
// It reconstructs just enough config for the substituter (Attach can't
// reproduce the full ResolvedConfig — the source devcontainer.json may
// have changed since Up; callers that need it should Resolve again),
// folds in the image's merged-config metadata label so callers see the
// same RemoteUser / lifecycle hooks / probe config as Up, and re-probes
// userEnv so subsequent Exec calls see the user's rc-file PATH additions.
//
// id stamps the workspace and cfg.DevcontainerID; localEnv may be nil
// (falls back to os.Environ()).
func (e *Engine) reattachWorkspace(ctx context.Context, details *runtime.ContainerDetails, id WorkspaceID, localEnv map[string]string) *Workspace {
	cfg := configFromContainerLabels(details)
	cfg.DevcontainerID = string(id)

	// Reading or parsing the metadata label is best-effort: failures
	// leave baseLayers nil and we fall back to the minimal cfg.
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
	if localEnv == nil {
		localEnv = environAsMap(os.Environ())
	}

	config.MergeMetadata(cfg, config.SubstitutionContext{
		LocalWorkspaceFolder:     cfg.LocalWorkspaceFolder,
		ContainerWorkspaceFolder: cfg.ContainerWorkspaceFolder,
		DevcontainerID:           cfg.DevcontainerID,
		LocalEnv:                 localEnv,
	}, baseLayers)
	cfg.Finalize()

	ws := &Workspace{
		ID:        id,
		Config:    cfg,
		Container: details,
		subst:     newSubstituter(cfg, details, localEnv),
	}

	if probed, err := e.probeUserEnv(ctx, ws, cfg.UserEnvProbe); err == nil {
		ws.probedEnv = probed
	}
	return ws
}
