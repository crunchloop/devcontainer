package devcontainer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/compose"
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

	// ExtraMounts are appended to the mounts derived from devcontainer.json.
	// They layer on top of cfg.WorkspaceMount and cfg.Mounts and are
	// preserved across reattach (they only apply on fresh container
	// creation, since reattach inherits the original container's mounts).
	// For compose sources, only Type == runtime.MountBind entries are
	// honored — other mount types are silently dropped to match the
	// devcontainer.json `mounts` semantics.
	ExtraMounts []runtime.MountSpec

	// ExtraContainerEnv is merged into the container's environment, layered
	// on top of cfg.ContainerEnv. Entries here are baked into the container
	// at start time, so every subsequent exec — including lifecycle scripts
	// and feature install — inherits them. Use this for callers that need
	// to inject host-derived env (PATH overrides, proxy vars, short-lived
	// auth tokens) without mutating devcontainer.json.
	ExtraContainerEnv map[string]string
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
		// Supported in M4 / PR12.
	default:
		return nil, fmt.Errorf("unknown source kind")
	}

	id := WorkspaceID(cfg.DevcontainerID)
	existing, err := e.runtime.FindContainerByLabel(ctx, LabelDevcontainerID, string(id))
	if err != nil {
		return nil, fmt.Errorf("find existing container: %w", err)
	}

	_, isCompose := cfg.Source.(*config.ComposeSource)

	if existing != nil && opts.Recreate {
		if isCompose {
			if err := e.composeDownExisting(ctx, existing); err != nil {
				return nil, err
			}
		} else {
			if err := e.removeContainer(ctx, existing.ID); err != nil {
				return nil, err
			}
		}
		existing = nil
	}

	var ws *Workspace
	switch {
	case isCompose:
		// `docker compose up -d` is idempotent: already-running services
		// stay, stopped ones restart, missing ones get created. So we
		// always go through createFreshCompose, regardless of whether
		// `existing` was found — compose handles the dispatch internally.
		ws, err = e.createFreshCompose(ctx, cfg, opts)
	case existing != nil:
		ws, err = e.attachExisting(ctx, existing, cfg, opts)
	default:
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

	spec := buildRunSpec(cfg, finalImage, opts.ExtraMounts, opts.ExtraContainerEnv)
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
// composeProjectName returns the deterministic compose project name
// for a workspace: `dc-<devcontainerId>` per PRD §12.5.
func composeProjectName(cfg *config.ResolvedConfig) string {
	return "dc-" + cfg.DevcontainerID
}

// composeWorkingDir picks the directory `docker compose` should run
// in. Compose interprets relative paths in the user's compose file
// (build contexts, env_file paths, volumes) relative to this. Default:
// the directory containing the source devcontainer.json. Falls back
// to LocalWorkspaceFolder when ConfigPath isn't supplied (Resolve's
// auto-discovery path).
func composeWorkingDir(cfg *config.ResolvedConfig, opts UpOptions) string {
	if opts.ConfigPath != "" {
		return filepath.Dir(opts.ConfigPath)
	}
	return filepath.Join(cfg.LocalWorkspaceFolder, ".devcontainer")
}

// createFreshCompose handles the *ComposeSource path of Up. It loads
// the user's compose project, picks the primary service, prepares the
// service's base image (pull or build), layers features atop it, writes
// our two override files, and runs `docker compose up -d` against the
// combined file list. Returns a Workspace whose Container is the
// primary service's container as resolved via `docker compose ps -q`.
func (e *Engine) createFreshCompose(ctx context.Context, cfg *config.ResolvedConfig, opts UpOptions) (*Workspace, error) {
	cr, ok := e.runtime.(runtime.ComposeRuntime)
	if !ok {
		return nil, fmt.Errorf("compose source: runtime does not support compose: %w", runtime.ErrNotImplemented)
	}

	src := cfg.Source.(*config.ComposeSource)
	if src.Service == "" {
		return nil, fmt.Errorf("compose source: devcontainer.json must specify \"service\"")
	}

	workingDir := composeWorkingDir(cfg, opts)
	projectName := composeProjectName(cfg)

	envList := mapToEnvList(opts.LocalEnv)
	project, err := compose.Load(ctx, compose.LoadOptions{
		Files:       src.Files,
		WorkingDir:  workingDir,
		ProjectName: projectName,
		Env:         envList,
	})
	if err != nil {
		return nil, err
	}

	primary, err := compose.PrimaryService(project, src.Service)
	if err != nil {
		return nil, err
	}

	baseImage, err := e.prepareComposeServiceImage(ctx, cfg, primary, workingDir, opts)
	if err != nil {
		return nil, err
	}

	finalImage, err := e.layerFeatures(ctx, cfg, baseImage, opts)
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp("", "dc-go-compose-*")
	if err != nil {
		return nil, fmt.Errorf("create compose override tmpdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	buildOverridePath := filepath.Join(tmp, "dc-build.yaml")
	runOverridePath := filepath.Join(tmp, "dc-run.yaml")

	if err := compose.WriteBuildOverride(buildOverridePath, compose.Override{
		Service: src.Service,
		Image:   finalImage,
	}); err != nil {
		return nil, err
	}

	bindMounts := []compose.BindMount{
		{Source: cfg.LocalWorkspaceFolder, Target: cfg.ContainerWorkspaceFolder},
	}
	for _, m := range cfg.Mounts {
		if m.Type == config.MountBind && m.Source != "" && m.Target != "" {
			bindMounts = append(bindMounts, compose.BindMount{
				Source:   m.Source,
				Target:   m.Target,
				ReadOnly: m.ReadOnly,
			})
		}
	}
	// Extra mounts: only bind types are expressible in compose overrides.
	// Other types (volume, tmpfs) are silently dropped, mirroring how
	// devcontainer.json `mounts` are filtered above.
	for _, m := range opts.ExtraMounts {
		if m.Type == runtime.MountBind && m.Source != "" && m.Target != "" {
			bindMounts = append(bindMounts, compose.BindMount{
				Source:   m.Source,
				Target:   m.Target,
				ReadOnly: m.ReadOnly,
			})
		}
	}

	if err := compose.WriteRunOverride(runOverridePath, project, compose.Override{
		Service:          src.Service,
		ExtraBindMounts:  bindMounts,
		ExtraEnvironment: mergeEnv(cfg.ContainerEnv, opts.ExtraContainerEnv),
		Labels: map[string]string{
			LabelDevcontainerID:       cfg.DevcontainerID,
			LabelLocalWorkspaceFolder: cfg.LocalWorkspaceFolder,
			LabelEngine:               engineIdent,
		},
	}); err != nil {
		return nil, err
	}

	allFiles := append([]string{}, src.Files...)
	allFiles = append(allFiles, buildOverridePath, runOverridePath)

	if err := cr.ComposeUp(ctx, runtime.ComposeUpSpec{
		Files:       allFiles,
		ProjectName: projectName,
		Services:    src.RunServices,
		WorkingDir:  workingDir,
	}, opts.Events); err != nil {
		return nil, err
	}

	containerID, err := cr.ComposeContainerID(ctx, runtime.ComposePsSpec{
		Files:       allFiles,
		ProjectName: projectName,
		WorkingDir:  workingDir,
	}, src.Service)
	if err != nil {
		return nil, fmt.Errorf("resolve compose primary container: %w", err)
	}
	if containerID == "" {
		return nil, fmt.Errorf("compose primary service %q has no running container after up", src.Service)
	}

	return e.buildWorkspace(ctx, containerID, cfg, opts.LocalEnv)
}

// prepareComposeServiceImage resolves the base image for a compose
// primary service: either the service's `image:` directive (pulled if
// missing locally) or the result of building its `build:` directive.
// Mirrors the image/build dispatch in prepareBaseImage but operates on
// the compose-go ServiceConfig.
func (e *Engine) prepareComposeServiceImage(ctx context.Context, cfg *config.ResolvedConfig, svc *composetypes.ServiceConfig, workingDir string, opts UpOptions) (string, error) {
	if svc.Image != "" && svc.Build == nil {
		if err := e.ensureImage(ctx, svc.Image, opts.PullPolicy, opts.Events); err != nil {
			return "", err
		}
		return svc.Image, nil
	}
	if svc.Build != nil {
		tag := "dc-go-compose-base-" + cfg.DevcontainerID + ":latest"
		ctxPath := svc.Build.Context
		if ctxPath == "" {
			ctxPath = workingDir
		}
		if !filepath.IsAbs(ctxPath) {
			ctxPath = filepath.Join(workingDir, ctxPath)
		}
		_, err := e.runtime.BuildImage(ctx, runtime.BuildSpec{
			ContextPath: ctxPath,
			Dockerfile:  svc.Build.Dockerfile,
			Tag:         tag,
			Args:        flattenStringMap(svc.Build.Args),
			Target:      svc.Build.Target,
		}, opts.Events)
		if err != nil {
			return "", fmt.Errorf("build compose primary service image: %w", err)
		}
		return tag, nil
	}
	return "", fmt.Errorf("compose primary service has neither image: nor build:")
}

// inspectStable is buildWorkspace's wrapper around InspectContainer that
// tolerates Docker's eventually-consistent state field after a recent
// start. The container's runtime state in Inspect updates asynchronously
// from the daemon's internal state machine; on slower runners we'd see
// "exited" briefly even after StartContainer's waitForRunning settled
// on "running". Retry a few times with backoff to absorb that lag.
func (e *Engine) inspectStable(ctx context.Context, id string) (*runtime.ContainerDetails, error) {
	const attempts = 5
	backoff := 50 * time.Millisecond
	var details *runtime.ContainerDetails
	var err error
	for i := 0; i < attempts; i++ {
		details, err = e.runtime.InspectContainer(ctx, id)
		if err != nil {
			return nil, err
		}
		if details.State == runtime.StateRunning {
			return details, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < time.Second {
			backoff *= 2
		}
	}
	// Last attempt's details are returned even if state isn't running —
	// caller may still use the workspace for diagnostics, and the
	// stored State will reflect what we observed.
	return details, nil
}

// flattenStringMap turns compose-go's MappingWithEquals (map[string]*string)
// into a plain map[string]string by dereferencing non-nil values.
func flattenStringMap(m composetypes.MappingWithEquals) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v != nil {
			out[k] = *v
		}
	}
	return out
}

// mapToEnvList converts a map to KEY=value strings sorted by key for
// deterministic input to compose-go's interpolation pass.
func mapToEnvList(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// composeDownExisting tears down a running compose project found by
// label scan during a Recreate-mode Up. We extract the project name
// from the existing container's compose label.
func (e *Engine) composeDownExisting(ctx context.Context, existing *runtime.Container) error {
	cr, ok := e.runtime.(runtime.ComposeRuntime)
	if !ok {
		// Fallback: just remove the single container we found.
		return e.removeContainer(ctx, existing.ID)
	}
	// Inspect to read labels — Container struct doesn't carry them.
	details, err := e.runtime.InspectContainer(ctx, existing.ID)
	if err != nil {
		return e.removeContainer(ctx, existing.ID)
	}
	projectName := details.Labels["com.docker.compose.project"]
	if projectName == "" {
		return e.removeContainer(ctx, existing.ID)
	}
	if err := cr.ComposeDown(ctx, runtime.ComposeDownSpec{
		ProjectName:   projectName,
		RemoveVolumes: false,
	}); err != nil {
		return fmt.Errorf("compose down for recreate: %w", err)
	}
	return nil
}

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
	details, err := e.inspectStable(ctx, containerID)
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
// additional mounts from devcontainer.json. extraMounts are appended after
// the cfg-derived mounts; extraEnv is merged into cfg.ContainerEnv (extras
// win on key collision). Labels are populated for label-based lookup.
func buildRunSpec(cfg *config.ResolvedConfig, image string, extraMounts []runtime.MountSpec, extraEnv map[string]string) runtime.RunSpec {
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
	mounts = append(mounts, extraMounts...)

	env := mergeEnv(cfg.ContainerEnv, extraEnv)

	return runtime.RunSpec{
		Image:           image,
		Name:            containerName(WorkspaceID(cfg.DevcontainerID)),
		User:            cfg.ContainerUser,
		WorkingDir:      cfg.ContainerWorkspaceFolder,
		Env:             env,
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

// mergeEnv returns a fresh map containing every entry in base, with extras
// applied on top. Returns nil if both inputs are empty so we don't allocate
// for the common no-extras path.
func mergeEnv(base, extras map[string]string) map[string]string {
	if len(base) == 0 && len(extras) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(extras))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extras {
		out[k] = v
	}
	return out
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
