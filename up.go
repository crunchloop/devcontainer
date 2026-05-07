package devcontainer

import (
	"context"
	"fmt"
	"os"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

// UpOptions configures Engine.Up.
type UpOptions struct {
	// LocalWorkspaceFolder is the absolute host path to the project. Required.
	LocalWorkspaceFolder string

	// ConfigPath is the absolute path to devcontainer.json. If empty,
	// discovered under LocalWorkspaceFolder per Resolve's rules.
	ConfigPath string

	// LocalEnv overrides os.Environ() for ${localEnv:*} resolution.
	// Nil means use the current process environment.
	LocalEnv map[string]string

	// Recreate, when true, stops + removes any existing container with our
	// label and creates a fresh one. Default false: an existing stopped
	// container is restarted (preserving in-container state); an existing
	// running container is attached to.
	Recreate bool

	// PullPolicy controls image pulling. Default IfNotPresent.
	PullPolicy PullPolicy

	// Events optionally receives runtime build/pull progress messages.
	// Drop-on-full; non-blocking.
	Events chan<- runtime.BuildEvent
}

// PullPolicy controls when images are pulled from a registry.
type PullPolicy string

const (
	PullIfNotPresent PullPolicy = "" // default
	PullAlways       PullPolicy = "always"
	PullNever        PullPolicy = "never"
)

// Up resolves the workspace's devcontainer.json, ensures its container is
// running, and returns a *Workspace ready for Exec.
//
// Re-attach semantics:
//   - existing running container with our label → attach (no restart)
//   - existing stopped container → restart, attach
//   - existing + UpOptions.Recreate=true → stop + remove, fresh create
//   - no existing container → fresh create
//
// Image source only in this milestone. Build / compose return
// runtime.ErrNotImplemented.
func (e *Engine) Up(ctx context.Context, opts UpOptions) (*Workspace, error) {
	if err := ctxIfDone(ctx); err != nil {
		return nil, err
	}
	if opts.LocalWorkspaceFolder == "" {
		return nil, fmt.Errorf("UpOptions.LocalWorkspaceFolder is required")
	}

	cfg, err := Resolve(ctx, ResolveOptions{
		LocalWorkspaceFolder: opts.LocalWorkspaceFolder,
		ConfigPath:           opts.ConfigPath,
		LocalEnv:             opts.LocalEnv,
	})
	if err != nil {
		return nil, err
	}

	switch cfg.Source.(type) {
	case *config.ImageSource:
		// Supported in M2.
	case *config.BuildSource:
		return nil, errBuildSourceNotImplemented
	case *config.ComposeSource:
		return nil, errComposeSourceNotImplemented
	default:
		return nil, fmt.Errorf("unknown source kind")
	}

	id := WorkspaceID(cfg.DevcontainerID)
	existing, err := e.runtime.FindContainerByLabel(ctx, LabelDevcontainerID, string(id))
	if err != nil {
		return nil, fmt.Errorf("find existing container: %w", err)
	}

	if existing != nil && opts.Recreate {
		if err := e.removeContainer(ctx, existing.ID); err != nil {
			return nil, err
		}
		existing = nil
	}

	if existing != nil {
		return e.attachExisting(ctx, existing, cfg, opts)
	}

	return e.createFresh(ctx, cfg, opts)
}

func (e *Engine) attachExisting(ctx context.Context, c *runtime.Container, cfg *config.ResolvedConfig, opts UpOptions) (*Workspace, error) {
	if c.State != runtime.StateRunning {
		if err := e.runtime.StartContainer(ctx, c.ID); err != nil {
			return nil, fmt.Errorf("start existing container %s: %w", c.ID, err)
		}
	}
	return e.buildWorkspace(ctx, c.ID, cfg, opts.LocalEnv)
}

func (e *Engine) createFresh(ctx context.Context, cfg *config.ResolvedConfig, opts UpOptions) (*Workspace, error) {
	imgSrc := cfg.Source.(*config.ImageSource)

	if err := e.ensureImage(ctx, imgSrc.Image, opts.PullPolicy, opts.Events); err != nil {
		return nil, err
	}

	spec := buildRunSpec(cfg, imgSrc.Image)
	c, err := e.runtime.RunContainer(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	if err := e.runtime.StartContainer(ctx, c.ID); err != nil {
		// Best-effort cleanup so we don't leave a created-but-stopped
		// container behind on hard failures.
		_ = e.runtime.RemoveContainer(ctx, c.ID, runtime.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start container %s: %w", c.ID, err)
	}
	return e.buildWorkspace(ctx, c.ID, cfg, opts.LocalEnv)
}

func (e *Engine) ensureImage(ctx context.Context, ref string, policy PullPolicy, events chan<- runtime.BuildEvent) error {
	switch policy {
	case PullAlways:
		_, err := e.runtime.PullImage(ctx, ref, events)
		return err
	case PullNever:
		// Caller asserts the image is local; rely on RunContainer to
		// return ImageNotFoundError if not.
		return nil
	default:
		// IfNotPresent: try inspect via pull-on-miss. The Docker daemon
		// short-circuits ImagePull when the image already exists locally
		// and immediately reports a "Pull complete" status, so this is
		// effectively a no-op for cached images. Any error during pull
		// surfaces here cleanly rather than at create time.
		_, err := e.runtime.PullImage(ctx, ref, events)
		return err
	}
}

func (e *Engine) removeContainer(ctx context.Context, id string) error {
	_ = e.runtime.StopContainer(ctx, id, runtime.StopOptions{})
	if err := e.runtime.RemoveContainer(ctx, id, runtime.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove existing container %s: %w", id, err)
	}
	return nil
}

func (e *Engine) buildWorkspace(ctx context.Context, containerID string, cfg *config.ResolvedConfig, localEnv map[string]string) (*Workspace, error) {
	details, err := e.runtime.InspectContainer(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", containerID, err)
	}
	if localEnv == nil {
		localEnv = environAsMap(os.Environ())
	}
	return &Workspace{
		ID:        WorkspaceID(cfg.DevcontainerID),
		Config:    cfg,
		Container: details,
		subst:     newSubstituter(cfg, details, localEnv),
	}, nil
}

// buildRunSpec converts a ResolvedConfig (image source) into a runtime.RunSpec.
// Mounts include the workspace bind (default or user-overridden) and any
// additional mounts from devcontainer.json. Labels are populated for
// label-based lookup.
func buildRunSpec(cfg *config.ResolvedConfig, image string) runtime.RunSpec {
	labels := map[string]string{
		LabelDevcontainerID:       cfg.DevcontainerID,
		LabelLocalWorkspaceFolder: cfg.LocalWorkspaceFolder,
		LabelEngine:               engineIdent,
	}
	// Container workspace folder is recoverable from inspect (WorkingDir),
	// so we don't bother labeling it. ConfigPath is only known to Up's
	// caller; we don't have a clean way to recover it inside the
	// container, so it stays in the (host-side) ResolvedConfig instead.

	mounts := buildMounts(cfg)

	return runtime.RunSpec{
		Image:           image,
		Name:            containerName(WorkspaceID(cfg.DevcontainerID)),
		User:            cfg.ContainerUser,
		WorkingDir:      cfg.ContainerWorkspaceFolder,
		Env:             cfg.ContainerEnv,
		Labels:          labels,
		Mounts:          mounts,
		RunArgs:         cfg.RunArgs,
		Init:            cfg.Init,
		Privileged:      cfg.Privileged,
		CapAdd:          cfg.CapAdd,
		SecurityOpt:     cfg.SecurityOpt,
		OverrideCommand: cfg.OverrideCommand,
	}
}

// buildMounts assembles the workspace bind plus any additional mounts. The
// workspace mount is the resolved cfg.WorkspaceMount if set, otherwise a
// default bind (LocalWorkspaceFolder → ContainerWorkspaceFolder, with
// "consistent" propagation on non-Linux per design/runtime.md §13.5).
func buildMounts(cfg *config.ResolvedConfig) []runtime.MountSpec {
	mounts := make([]runtime.MountSpec, 0, len(cfg.Mounts)+1)

	mounts = append(mounts, defaultWorkspaceMount(cfg))

	for _, m := range cfg.Mounts {
		mounts = append(mounts, runtime.MountSpec{
			Type:     runtime.MountType(m.Type),
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return mounts
}
