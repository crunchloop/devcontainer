package devcontainer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/feature"
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

	// SkipLifecycle, when true, suppresses automatic invocation of
	// devcontainer lifecycle phases (onCreate, postCreate, etc.) from
	// Up. Phases can still be run explicitly via Engine.RunLifecycle.
	// Default false: Up runs the full configured lifecycle.
	SkipLifecycle bool

	// RunInitializeCommand, when true, runs the host-side initializeCommand
	// before container creation. Default false because the spec lets
	// devcontainer.json execute arbitrary host commands; opt-in only.
	// Note: v1 initialize execution is a stub that returns an error;
	// real host execution requires caller-supplied wiring (PRD §11).
	RunInitializeCommand bool
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
		// Supported in M3 / PR8.
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

	var ws *Workspace
	if existing != nil {
		ws, err = e.attachExisting(ctx, existing, cfg, opts)
	} else {
		ws, err = e.createFresh(ctx, cfg, opts)
	}
	if err != nil {
		return nil, err
	}

	if !opts.SkipLifecycle {
		if err := e.runAllLifecycle(ctx, ws, opts.RunInitializeCommand); err != nil {
			return nil, err
		}
	}
	return ws, nil
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
	baseImage, err := e.prepareBaseImage(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}

	finalImage, err := e.layerFeatures(ctx, cfg, baseImage, opts)
	if err != nil {
		return nil, err
	}

	spec := buildRunSpec(cfg, finalImage)
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

// markAlreadyInstalled flips AlreadyInstalled on each cfg.Features
// entry whose request is satisfied by an entry in the base image's
// devcontainer.metadata label. The match strategy (permissive vs
// strict) follows EngineOptions.StrictFeatureVersionMatch.
//
// Pre-baked-image matching only applies to OCI features: the bare id
// comparison requires extracting a basename from the request ref,
// which is reliable for OCI (e.g. ghcr.io/x/foo:1 → "foo") but not for
// HTTPS URLs or local paths whose disk-side id may differ from the
// ref's last path component. Local / HTTPS features always go through
// fetch (cheap for local; HTTPS fetch is what gives us the id anyway).
func (e *Engine) markAlreadyInstalled(cfg *config.ResolvedConfig, baked []config.FeatureMetadata) {
	mode := feature.MatchPermissive
	if e.opts.StrictFeatureVersionMatch {
		mode = feature.MatchStrict
	}
	for i := range cfg.Features {
		f := &cfg.Features[i]
		if f.AlreadyInstalled {
			continue
		}
		if f.SourceKind != config.FeatureSourceOCI {
			continue
		}
		req := config.FeatureMetadata{
			ID:      ociRefBareID(f.Ref),
			Version: optionVersionString(f.Options),
		}
		if req.ID == "" {
			continue
		}
		for _, b := range baked {
			if feature.Matches(b, req, mode) {
				f.AlreadyInstalled = true
				// Preserve baked metadata so the regenerated label can
				// include it without re-fetching.
				f.Metadata = b
				break
			}
		}
	}
}

// ociRefBareID extracts the bare feature id from an OCI ref:
//
//	ghcr.io/devcontainers/features/git:1   → git
//	ghcr.io/owner/feature@sha256:...       → feature
//	ghcr.io/owner/feature                  → feature
//
// Returns "" for malformed inputs.
func ociRefBareID(ref string) string {
	// Strip @digest and :tag.
	if i := lastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := lastIndex(ref, ":"); i >= 0 {
		if !contains(ref[:i], "://") || lastIndex(ref, "/") > i {
			ref = ref[:i]
		}
	}
	// Take the segment after the last "/".
	if i := lastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func optionVersionString(opts map[string]any) string {
	if v, ok := opts["version"].(string); ok {
		return v
	}
	return ""
}

func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// prepareBaseImage produces the image reference to use as the base for
// feature layering: pulled (image source) or built (build source).
func (e *Engine) prepareBaseImage(ctx context.Context, cfg *config.ResolvedConfig, opts UpOptions) (string, error) {
	switch s := cfg.Source.(type) {
	case *config.ImageSource:
		if err := e.ensureImage(ctx, s.Image, opts.PullPolicy, opts.Events); err != nil {
			return "", err
		}
		return s.Image, nil

	case *config.BuildSource:
		tag := "dc-go-base-" + cfg.DevcontainerID + ":latest"
		_, err := e.runtime.BuildImage(ctx, runtime.BuildSpec{
			ContextPath: s.Context,
			Dockerfile:  s.Dockerfile,
			Tag:         tag,
			Args:        s.Args,
			Target:      s.Target,
			CacheFrom:   s.CacheFrom,
		}, opts.Events)
		if err != nil {
			return "", fmt.Errorf("build base image from %s: %w", s.Context, err)
		}
		return tag, nil

	default:
		return "", fmt.Errorf("unsupported source kind for base image: %T", cfg.Source)
	}
}

// layerFeatures fetches each feature in cfg.Features (where not already
// flagged AlreadyInstalled), re-orders by full DAG, generates a
// feature-extending Dockerfile, and builds it. Returns the tag of the
// final image. If no features need installing, returns baseImage as-is.
//
// Side effect: cfg.Features entries are mutated in place to populate
// Dir, Metadata, ResolvedRef on fetch. Caller's Workspace.Config sees
// the post-fetch state.
func (e *Engine) layerFeatures(ctx context.Context, cfg *config.ResolvedConfig, baseImage string, opts UpOptions) (string, error) {
	if len(cfg.Features) == 0 {
		return baseImage, nil
	}

	// Pre-baked-image hot path: read the base image's
	// devcontainer.metadata label and mark already-installed features.
	// Failures (missing label, parse error, image not found) are
	// non-fatal — we just don't get the optimization.
	var baseMeta []config.FeatureMetadata
	if details, err := e.runtime.InspectImage(ctx, baseImage); err == nil && details != nil {
		if label := details.Labels[feature.MetadataLabel]; label != "" {
			parsed, err := feature.ParseMetadataLabel(label)
			if err == nil {
				baseMeta = parsed
				e.markAlreadyInstalled(cfg, parsed)
			}
		}
	}

	configDir := ""
	// LocalWorkspaceFolder is always populated by Resolve; ConfigPath
	// (where local features are relative to) lives only on the
	// ResolveOptions. We re-derive configDir from what the user
	// supplied via UpOptions; if neither is supplied use
	// LocalWorkspaceFolder/.devcontainer as a sensible fallback.
	if opts.ConfigPath != "" {
		configDir = filepath.Dir(opts.ConfigPath)
	} else {
		configDir = filepath.Join(cfg.LocalWorkspaceFolder, ".devcontainer")
	}

	// Fetch each not-yet-installed feature. Resolve refs in place.
	for i := range cfg.Features {
		f := &cfg.Features[i]
		if f.AlreadyInstalled || f.Dir != "" {
			continue
		}
		ref := f.Ref
		if f.SourceKind == config.FeatureSourceLocal {
			ref = feature.ResolveLocalRef(configDir, f.Ref)
		}
		fetched, err := e.featureStore.Fetch(ctx, ref, f.SourceKind)
		if err != nil {
			return "", fmt.Errorf("fetch feature %s: %w", f.Ref, err)
		}
		f.Dir = fetched.Dir
		f.ResolvedRef = fetched.ResolvedRef
		f.Metadata = fetched.Metadata

		// Apply spec defaults + validate against the now-known options.
		merged, mwarns, err := feature.MergeOptions(f.Metadata, f.Options)
		if err != nil {
			return "", fmt.Errorf("feature %s: %w", f.Ref, err)
		}
		f.Options = merged
		cfg.Warnings = append(cfg.Warnings, mwarns...)
	}

	// Re-order with fully-populated metadata so installsAfter / dependsOn apply.
	ordered, oWarns, err := feature.Order(cfg.Features, nil)
	if err != nil {
		return "", err
	}
	cfg.Features = ordered
	cfg.Warnings = append(cfg.Warnings, oWarns...)

	plan := feature.BuildPlan{
		BaseImage:         baseImage,
		Features:          cfg.Features,
		RemoteUser:        cfg.RemoteUser,
		ContainerUser:     cfg.ContainerUser,
		BaseImageMetadata: baseMeta,
	}
	if !plan.HasWork() {
		return baseImage, nil
	}

	tmp, err := os.MkdirTemp("", "dc-go-build-*")
	if err != nil {
		return "", fmt.Errorf("create build context tmpdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := feature.GenerateBuildContext(plan, tmp); err != nil {
		return "", fmt.Errorf("generate feature build context: %w", err)
	}

	finalTag := "dc-go-final-" + cfg.DevcontainerID + ":latest"
	_, err = e.runtime.BuildImage(ctx, runtime.BuildSpec{
		ContextPath: tmp,
		Dockerfile:  "Dockerfile",
		Tag:         finalTag,
		Args: map[string]string{
			"_DEV_CONTAINERS_BASE_IMAGE": baseImage,
		},
	}, opts.Events)
	if err != nil {
		return "", fmt.Errorf("build feature-extended image: %w", err)
	}
	return finalTag, nil
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
		// IfNotPresent: inspect first; only pull on miss. This avoids
		// hitting a registry for locally-built images (sha256:... and
		// dc-go-* tags from previous Ups) which would otherwise fail
		// since they don't exist remotely.
		if _, err := e.runtime.InspectImage(ctx, ref); err == nil {
			return nil
		}
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
