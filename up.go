package devcontainer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/compose"
	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
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

	// AdoptExisting, for compose sources, reuses any existing container
	// found for a (project, service) regardless of whether its stored
	// config-hash / image-digest still match — start it if stopped,
	// attach if running, never recreate. This is the RESUME contract:
	// reattach the workspace exactly as it was left, preserving every
	// container's writable upperdir and its (possibly anonymous) volumes,
	// rather than reconciling against config drift (a rebuilt
	// feature-layered primary image has a fresh digest every boot, which
	// otherwise forces a recreate that abandons the upperdir and binds a
	// new empty anonymous volume). Ignored when Recreate is true.
	AdoptExisting bool

	// PullPolicy controls image pulling. Default IfNotPresent.
	PullPolicy PullPolicy

	// Events optionally receives structured engine events for the duration
	// of this Up call (config resolved, feature resolve, build/pull
	// progress, container lifecycle, spec lifecycle phases). Drop-on-full;
	// the engine never blocks on send. See package events for the type
	// surface (experimental until v1.0.0).
	//
	// Ownership: the caller owns the channel. The engine only writes —
	// it never closes the channel. The caller MUST NOT close it before
	// Up returns; closing a channel while the engine is still sending
	// races with the engine's send and will panic. Close after Up
	// returns (or simply leave the channel open and let it be GC'd).
	Events chan<- events.Event

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

	// RunSecretsCommand, when true, runs the host-side secretsCommand
	// before container creation and merges its stdout (parsed as
	// key=value lines) into the container's environment. Default false
	// for the same reason as RunInitializeCommand: arbitrary host
	// execution is opt-in. Requires EngineOptions.HostExecutor to be
	// set; otherwise a *LifecycleError wrapping
	// ErrHostExecutorNotConfigured is returned.
	//
	// Only applied on fresh container creation. On reattach the
	// existing container's env is already baked, so re-running
	// secretsCommand would have no effect and we skip it; callers
	// wanting a refresh should pass Recreate=true.
	RunSecretsCommand bool

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

	// bus is the per-Up event bus, set at the top of Engine.Up and threaded
	// through internal helpers via the UpOptions value copy. Never read or
	// written by callers.
	bus *eventBus

	// override carries build-time knobs supplied by Engine.Build callers
	// (final image tag, --no-cache, --platform, additional --cache-from).
	// Engine.Up itself leaves this nil; prepareBaseImage and layerFeatures
	// merge override values into their runtime.BuildSpec when non-nil.
	override *buildOverride
}

// buildOverride is the bag of build-time knobs Engine.Build threads
// through Up's internal helpers. Internal-only — Engine.Build sets it,
// Engine.Up leaves it nil.
type buildOverride struct {
	// FinalTag, when non-empty, replaces the auto-generated tag of the
	// *last* BuildImage call in the chain. Which call that is depends
	// on BaseIsFinal: when true the base Dockerfile build is the last
	// step (BuildSource with no features); otherwise it's the features
	// build (any source with at least one feature).
	FinalTag    string
	BaseIsFinal bool
	// NoCache forces --no-cache on every BuildImage call in the chain.
	NoCache bool
	// Platform pins the target platform on every BuildImage call.
	Platform string
	// ExtraCacheFrom is appended to whatever CacheFrom the source
	// declares (devcontainer.json `build.cacheFrom`).
	ExtraCacheFrom []string
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

	opts.bus = newEventBus(e.emitter, opts.Events)
	defer opts.bus.Close()

	cfg, err := Resolve(ctx, ResolveOptions{
		LocalWorkspaceFolder: opts.LocalWorkspaceFolder,
		ConfigPath:           opts.ConfigPath,
		LocalEnv:             opts.LocalEnv,
	})
	if err != nil {
		return nil, err
	}

	opts.bus.Emit(events.ConfigResolvedEvent{Config: cfg})
	for _, w := range cfg.Warnings {
		opts.bus.Emit(events.ConfigWarningEvent{Code: string(w.Code), Message: w.Message})
	}
	// Track how many warnings exist post-Resolve. layerFeatures may
	// append more (feature option validation, DAG depth, post-fetch
	// re-Order) — we emit ConfigWarningEvent for those after the
	// workspace is ready so callers see them in the event stream and
	// not just on Workspace.Config.Warnings.
	preFeatureWarnings := len(cfg.Warnings)

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
		//
		// Reattach caveat for host-side hooks: when compose finds an
		// existing container, its env is already baked. secretsCommand
		// output would not actually flow into the running container,
		// so suppress it here to keep the "fresh-only" contract
		// documented on UpOptions.RunSecretsCommand. Callers wanting a
		// refresh pass Recreate=true (which already nils out
		// `existing` above).
		composeOpts := opts
		if existing != nil {
			composeOpts.RunSecretsCommand = false
		}
		ws, err = e.createFreshCompose(ctx, cfg, composeOpts, existing != nil)
	case existing != nil:
		ws, err = e.attachExisting(ctx, existing, cfg, opts)
	default:
		ws, err = e.createFresh(ctx, cfg, opts)
	}
	if err != nil {
		return nil, err
	}

	// Emit any warnings appended between Resolve and now (feature
	// options validation, DAG depth, post-fetch re-Order). Done after
	// the workspace exists so consumers can correlate via the bus's
	// Seq with container.created / container.started.
	if ws != nil && ws.Config != nil {
		for _, w := range ws.Config.Warnings[preFeatureWarnings:] {
			opts.bus.Emit(events.ConfigWarningEvent{Code: string(w.Code), Message: w.Message})
		}
	}

	// Probe BEFORE lifecycle so hooks (postCreateCommand etc.) see the
	// PATH and other vars contributed by the user's shell rc files —
	// e.g. nvm/asdf-managed binaries, or pnpm installed via the
	// devcontainers-extra/pnpm feature, which only publishes its bin
	// dir via an rc-snippet sourced by bash -l. This matches the
	// @devcontainers/cli reference: it awaits the probe promise before
	// running every lifecycle exec. Failures are non-fatal.
	if probed, err := e.probeUserEnv(ctx, ws, ws.Config.UserEnvProbe); err == nil {
		ws.probedEnv = probed
	}
	var lifecycleErr error
	if !opts.SkipLifecycle {
		lifecycleErr = e.runAllLifecycle(ctx, ws, opts.RunInitializeCommand, opts.bus)
	}
	// Re-probe AFTER lifecycle: hooks may have installed rc-modifying
	// tools themselves (the classic case the prior comment described),
	// and subsequent Exec calls should see those additions too. Done
	// even when lifecycle failed — partial rc edits made before the
	// failing hook should still reach the caller's recovery Exec calls.
	if probed, err := e.probeUserEnv(ctx, ws, ws.Config.UserEnvProbe); err == nil {
		ws.probedEnv = probed
	}
	// On *lifecycle* failure (postCreateCommand etc. exited non-zero)
	// return (ws, err) instead of (nil, err). The container is created
	// and still running; consumers building VS Code / Codespaces-style
	// UX want to surface a warning and keep the container reattachable
	// rather than treat every postCreateCommand bug as a fatal Up
	// failure. Matches @devcontainers/cli, which exits 1 on lifecycle
	// failure but leaves the container intact.
	//
	// Other errors out of runAllLifecycle (marker read/write I/O,
	// ctx cancellation) still return (nil, err): they're not the
	// user-script-failed case the partial-success contract targets,
	// and leaking a workspace handle there would surprise strict
	// callers. *LifecycleError is the discriminator.
	if lifecycleErr != nil {
		if IsLifecycleError(lifecycleErr) {
			return ws, lifecycleErr
		}
		return nil, lifecycleErr
	}
	return ws, nil
}

func (e *Engine) attachExisting(ctx context.Context, c *runtime.Container, cfg *config.ResolvedConfig, opts UpOptions) (*Workspace, error) {
	if c.State != runtime.StateRunning {
		if err := e.runtime.StartContainer(ctx, c.ID); err != nil {
			return nil, fmt.Errorf("start existing container %s: %w", c.ID, err)
		}
		opts.bus.Emit(events.ContainerStartedEvent{ContainerID: c.ID, StartedAt: time.Now()})
	}
	// The container's image carries the merged-config metadata label
	// from the prior build; re-running the merge here keeps cfg's
	// remoteUser / lifecycle hooks / env consistent across reattach,
	// not just on fresh creation.
	var baseLayers []config.FeatureMetadata
	if details, err := e.runtime.InspectImage(ctx, c.Image); err == nil && details != nil {
		if label := details.Labels[feature.MetadataLabel]; label != "" {
			if parsed, err := feature.ParseMetadataLabel(label); err == nil {
				baseLayers = parsed
			}
		}
	}
	applyMetadataMerge(cfg, baseLayers, opts.LocalEnv)
	return e.buildWorkspace(ctx, c.ID, cfg, opts.LocalEnv)
}

func (e *Engine) createFresh(ctx context.Context, cfg *config.ResolvedConfig, opts UpOptions) (*Workspace, error) {
	baseImage, err := e.prepareBaseImage(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}

	finalImage, baseLayers, err := e.layerFeatures(ctx, cfg, baseImage, opts)
	if err != nil {
		return nil, err
	}
	applyMetadataMerge(cfg, baseLayers, opts.LocalEnv)

	finalImage, err = e.reconcileRemoteUserUID(ctx, cfg, finalImage, opts)
	if err != nil {
		return nil, err
	}

	extraEnv, err := e.collectSecretsEnv(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}

	spec := buildRunSpec(cfg, finalImage, opts.ExtraMounts, extraEnv)
	opts.bus.Emit(events.ContainerCreatingEvent{Name: spec.Name})
	c, err := e.runtime.RunContainer(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	opts.bus.Emit(events.ContainerCreatedEvent{ContainerID: c.ID, Name: spec.Name})
	if err := e.runtime.StartContainer(ctx, c.ID); err != nil {
		// Best-effort cleanup so we don't leave a created-but-stopped
		// container behind on hard failures.
		_ = e.runtime.RemoveContainer(ctx, c.ID, runtime.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start container %s: %w", c.ID, err)
	}
	opts.bus.Emit(events.ContainerStartedEvent{ContainerID: c.ID, StartedAt: time.Now()})
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

// emitFeatureSkippedFromLabel fires FeatureSkipped events for each request
// that markAlreadyInstalled flipped via the base image's label. Called
// from layerFeatures after the mark pass.
func emitFeatureSkippedFromLabel(bus *eventBus, features []config.ResolvedFeature) {
	for _, f := range features {
		if f.AlreadyInstalled {
			bus.Emit(events.FeatureSkippedEvent{Ref: f.Ref, Reason: "base_image_label"})
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
// service's base image (pull or build), layers features atop it, and
// — depending on EngineOptions.ComposeBackend — either:
//
//   - Shellout (default): writes our two override files and runs
//     `docker compose up -d` via runtime.ComposeRuntime.
//   - Native: mutates the loaded *types.Project in memory via
//     ApplyBuildOverride / ApplyRunOverride, then drives
//     compose.Orchestrator.Up against runtime.Runtime primitives.
//
// Returns a Workspace whose Container is the primary service's
// container.
func (e *Engine) createFreshCompose(ctx context.Context, cfg *config.ResolvedConfig, opts UpOptions, existingContainer bool) (*Workspace, error) {
	// Shellout backend requires the runtime to satisfy
	// ComposeRuntime. Check before any project load / image work so a
	// missing capability fails fast with ErrNotImplemented instead of
	// after parse errors. Native backend uses runtime.Runtime
	// directly; primitive support is enforced by Plan.Validate +
	// surface-level ErrNotImplemented from the backend.
	if e.opts.ComposeBackend == ComposeBackendShellout {
		if _, ok := e.runtime.(runtime.ComposeRuntime); !ok {
			return nil, fmt.Errorf("compose source: runtime does not support compose: %w", runtime.ErrNotImplemented)
		}
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

	if err := e.refuseUnsupportedComposeProject(project, projectName); err != nil {
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

	finalImage, baseLayers, err := e.layerFeatures(ctx, cfg, baseImage, opts)
	if err != nil {
		return nil, err
	}
	applyMetadataMerge(cfg, baseLayers, opts.LocalEnv)

	finalImage, err = e.reconcileRemoteUserUID(ctx, cfg, finalImage, opts)
	if err != nil {
		return nil, err
	}

	extraEnv, err := e.collectSecretsEnv(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}

	bindMounts := composeBindMounts(cfg, opts)

	overrideLabels := map[string]string{
		LabelDevcontainerID:       cfg.DevcontainerID,
		LabelLocalWorkspaceFolder: cfg.LocalWorkspaceFolder,
		LabelEngine:               engineIdent,
	}
	overrideEnv := mergeEnv(cfg.ContainerEnv, extraEnv)

	// When features/image metadata declare entrypoint scripts (e.g.
	// docker-in-docker's docker-init.sh), they must run before the
	// service's command — see RenderEntrypointWrapper. The wrapper
	// preserves the "original" entrypoint underneath: the service's own
	// `entrypoint:` if it declared one, else the image's ENTRYPOINT.
	var origEntrypoint []string
	if len(cfg.Entrypoints) > 0 {
		if svc, ok := project.Services[src.Service]; ok && len(svc.Entrypoint) > 0 {
			origEntrypoint = []string(svc.Entrypoint)
		} else if details, err := e.runtime.InspectImage(ctx, finalImage); err == nil && details != nil {
			origEntrypoint = details.Entrypoint
		} else if err != nil {
			// Non-fatal: we still apply the entrypoint wrapper, but the
			// image's own ENTRYPOINT can't be preserved underneath it.
			// Surface it so this silent fallback is diagnosable.
			opts.bus.Emit(events.WarnEvent{
				Code: "compose_entrypoint_image_inspect_failed",
				Message: fmt.Sprintf("could not inspect %s to preserve its ENTRYPOINT under the feature-entrypoint wrapper for service %q: %v",
					finalImage, src.Service, err),
			})
		}
	}

	// Run-time override layered onto the primary service. Mirrors the
	// flags the image path applies in newRunSpec: workspace/cfg mounts,
	// container env, convergence labels, the security options merged from
	// feature+image metadata (Privileged/Init/CapAdd/SecurityOpt), and the
	// feature-entrypoint chain — without these, features like
	// docker-in-docker that declare privileged/init/entrypoint silently
	// fail on compose-source devcontainers.
	runOverride := compose.Override{
		Service:            src.Service,
		ExtraBindMounts:    bindMounts,
		ExtraEnvironment:   overrideEnv,
		Labels:             overrideLabels,
		Privileged:         cfg.Privileged,
		Init:               cfg.Init,
		CapAdd:             cfg.CapAdd,
		SecurityOpt:        cfg.SecurityOpt,
		Entrypoints:        cfg.Entrypoints,
		OriginalEntrypoint: origEntrypoint,
	}

	switch e.opts.ComposeBackend {
	case ComposeBackendNative:
		return e.upComposeNative(ctx, cfg, opts, project, src, projectName,
			workingDir, finalImage, runOverride)
	case ComposeBackendShellout:
		return e.upComposeShellout(ctx, cfg, opts, project, src, projectName,
			workingDir, finalImage, runOverride, existingContainer)
	default:
		// Reject unknown values explicitly so a typo in
		// EngineOptions.ComposeBackend doesn't silently route to
		// shellout and require a compose-plugin install at runtime.
		return nil, fmt.Errorf("compose source: unknown ComposeBackend value %d", e.opts.ComposeBackend)
	}
}

// upComposeShellout is the legacy path: write the two override files
// and shell out via runtime.ComposeRuntime. Unchanged from M4.
func (e *Engine) upComposeShellout(
	ctx context.Context,
	cfg *config.ResolvedConfig,
	opts UpOptions,
	project *composetypes.Project,
	src *config.ComposeSource,
	projectName, workingDir, finalImage string,
	runOverride compose.Override,
	existingContainer bool,
) (*Workspace, error) {
	cr, ok := e.runtime.(runtime.ComposeRuntime)
	if !ok {
		return nil, fmt.Errorf("compose source: runtime does not support compose: %w", runtime.ErrNotImplemented)
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

	if err := compose.WriteRunOverride(runOverridePath, project, runOverride); err != nil {
		return nil, err
	}

	allFiles := append([]string{}, src.Files...)
	allFiles = append(allFiles, buildOverridePath, runOverridePath)

	// Mirror upstream devcontainers/cli: when a container is already
	// known to exist for this workspace id, pass --no-recreate so
	// `docker compose up` keeps the existing container instead of
	// destroying it on spurious config-hash drift (override file
	// reordering, pod-scoped env injected by callers, etc.). Loses
	// the container's writable layer otherwise — see issue #71.
	if err := cr.ComposeUp(ctx, runtime.ComposeUpSpec{
		Files:       allFiles,
		ProjectName: projectName,
		Services:    src.RunServices,
		WorkingDir:  workingDir,
		NoRecreate:  existingContainer,
	}, opts.bus.BuildChan(events.BuildSourceCompose)); err != nil {
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

// refuseUnsupportedComposeProject rejects a project the native
// orchestrator will not run, before anything is built or pulled.
//
// Orchestrator.Up validates again at the top of its own flow; that
// call stays as the authoritative one, and this is the same
// side-effect-free check hoisted ahead of the work. Without it the
// refusal lands only after primary-image preparation, feature
// layering and sidecar builds have already run, so a project we were
// never going to start still costs the user a build and leaves the
// tagged images behind.
//
// Native backend only. The shell-out backend hands the project to
// `docker compose`, which implements fields we refuse — validating
// there would reject projects that work today.
func (e *Engine) refuseUnsupportedComposeProject(project *composetypes.Project, projectName string) error {
	if e.opts.ComposeBackend != ComposeBackendNative {
		return nil
	}
	plan := &compose.Plan{Project: project, ProjectName: projectName}
	return plan.Validate(e.runtime.Capabilities())
}

// upComposeNative is the new path: mutate the project in-memory via
// the Apply* override helpers, then drive compose.Orchestrator.Up
// against the runtime's primitive surface. No tmpfile, no docker
// compose plugin, no runtime.ComposeRuntime assertion.
func (e *Engine) upComposeNative(
	ctx context.Context,
	cfg *config.ResolvedConfig,
	opts UpOptions,
	project *composetypes.Project,
	src *config.ComposeSource,
	projectName, workingDir, finalImage string,
	runOverride compose.Override,
) (*Workspace, error) {
	if err := compose.ApplyBuildOverride(project, src.Service, finalImage); err != nil {
		return nil, err
	}
	if err := e.buildComposeSidecarImages(ctx, project, src, projectName, workingDir, opts); err != nil {
		return nil, err
	}
	if err := compose.ApplyRunOverride(project, src.Service, runOverride); err != nil {
		return nil, err
	}

	orch := compose.NewOrchestrator(e.runtime)
	res, err := orch.Up(ctx, &compose.Plan{
		Project:       project,
		ProjectName:   projectName,
		Services:      src.RunServices,
		AdoptExisting: opts.AdoptExisting && !opts.Recreate,
		Labels: map[string]string{
			LabelDevcontainerID: cfg.DevcontainerID,
		},
	})
	if err != nil {
		return nil, err
	}
	containerID := res.ContainerIDs[src.Service]
	if containerID == "" {
		return nil, fmt.Errorf("compose primary service %q was not started by orchestrator", src.Service)
	}
	return e.buildWorkspace(ctx, containerID, cfg, opts.LocalEnv)
}

// composeBindMounts assembles the workspace + cfg + extra mounts in
// the order the engine has always used them, kept as a helper so
// both compose backends share the exact same set. Only bind mounts
// are expressible in compose overrides; other types are silently
// dropped — same as the legacy path.
func composeBindMounts(cfg *config.ResolvedConfig, opts UpOptions) []compose.BindMount {
	out := []compose.BindMount{
		{Source: cfg.LocalWorkspaceFolder, Target: cfg.ContainerWorkspaceFolder},
	}
	for _, m := range cfg.Mounts {
		if m.Type == config.MountBind && m.Source != "" && m.Target != "" {
			out = append(out, compose.BindMount{
				Source:   m.Source,
				Target:   m.Target,
				ReadOnly: m.ReadOnly,
			})
		}
	}
	for _, m := range opts.ExtraMounts {
		if m.Type == runtime.MountBind && m.Source != "" && m.Target != "" {
			out = append(out, compose.BindMount{
				Source:   m.Source,
				Target:   m.Target,
				ReadOnly: m.ReadOnly,
			})
		}
	}
	return out
}

// buildComposeSidecarImages builds every selected non-primary service
// that declares `build:`. The shellout backend delegated these to
// `docker compose up`, which builds missing images implicitly; the
// native orchestrator only creates containers from images, so the
// builds must happen before it runs. Compose semantics are preserved:
// a service with both `image:` and `build:` gets the built image
// tagged with its `image:`, a build-only service gets compose v2's
// default `<project>-<service>` name. Either way the service's
// `build:` is cleared and `image:` set, so the orchestrator's hash,
// pull-retry, and drift checks all see a concrete reference.
func (e *Engine) buildComposeSidecarImages(
	ctx context.Context,
	project *composetypes.Project,
	src *config.ComposeSource,
	projectName, workingDir string,
	opts UpOptions,
) error {
	// Selection mirrors `docker compose up <primary> <runServices...>`:
	// the named services plus their transitive dependencies (the same
	// closure the orchestrator keeps), so a dependency of a selected
	// service gets its image built even when runServices doesn't name
	// it directly.
	selected := func(string) bool { return true }
	if len(src.RunServices) > 0 {
		closure := compose.ServiceClosure(project,
			append(append([]string(nil), src.RunServices...), src.Service))
		selected = func(name string) bool { return slices.Contains(closure, name) }
	}
	// Deterministic build order: map iteration would shuffle build
	// output (and any failure) between runs.
	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := project.Services[name]
		// The primary service's build already happened in
		// prepareComposeServiceImage (with feature layering on top);
		// ApplyBuildOverride cleared its Build field before this runs.
		if name == src.Service || svc.Build == nil || !selected(name) {
			continue
		}
		tag := svc.Image
		if tag == "" {
			tag = projectName + "-" + name
		}
		ctxPath := svc.Build.Context
		if ctxPath == "" {
			ctxPath = workingDir
		}
		if !filepath.IsAbs(ctxPath) {
			ctxPath = filepath.Join(workingDir, ctxPath)
		}
		opts.bus.Emit(events.BuildStartEvent{Source: events.BuildSourceDockerfile, Ref: tag})
		if _, err := e.runtime.BuildImage(ctx, runtime.BuildSpec{
			ContextPath: ctxPath,
			Dockerfile:  svc.Build.Dockerfile,
			Tag:         tag,
			Args:        flattenStringMap(svc.Build.Args),
			Target:      svc.Build.Target,
		}, opts.bus.BuildChan(events.BuildSourceDockerfile)); err != nil {
			return fmt.Errorf("build compose service %q image: %w", name, err)
		}
		if err := compose.ApplyBuildOverride(project, name, tag); err != nil {
			return err
		}
	}
	return nil
}

// prepareComposeServiceImage resolves the base image for a compose
// primary service: either the service's `image:` directive (pulled if
// missing locally) or the result of building its `build:` directive.
// Mirrors the image/build dispatch in prepareBaseImage but operates on
// the compose-go ServiceConfig.
func (e *Engine) prepareComposeServiceImage(ctx context.Context, cfg *config.ResolvedConfig, svc *composetypes.ServiceConfig, workingDir string, opts UpOptions) (string, error) {
	if svc.Image != "" && svc.Build == nil {
		opts.bus.Emit(events.BuildStartEvent{Source: events.BuildSourceImage, Ref: svc.Image})
		if err := e.ensureImage(ctx, svc.Image, opts.PullPolicy, opts.bus.BuildChan(events.BuildSourceImage)); err != nil {
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
		opts.bus.Emit(events.BuildStartEvent{Source: events.BuildSourceDockerfile, Ref: tag})
		_, err := e.runtime.BuildImage(ctx, runtime.BuildSpec{
			ContextPath: ctxPath,
			Dockerfile:  svc.Build.Dockerfile,
			Tag:         tag,
			Args:        flattenStringMap(svc.Build.Args),
			Target:      svc.Build.Target,
		}, opts.bus.BuildChan(events.BuildSourceDockerfile))
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
//
// Dispatches to the same backend as Up: native callers MUST NOT
// fall through to ComposeRuntime.ComposeDown (it would shell out to
// docker compose, which the native flag is meant to avoid, AND it
// won't see services the orchestrator created without the shellout
// path's project layout).
func (e *Engine) composeDownExisting(ctx context.Context, existing *runtime.Container) error {
	// Inspect to read labels — Container struct populates them on
	// the FindContainerByLabel path but not all callers.
	details, err := e.runtime.InspectContainer(ctx, existing.ID)
	if err != nil {
		return e.removeContainer(ctx, existing.ID)
	}
	projectName := details.Labels["com.docker.compose.project"]
	if projectName == "" {
		return e.removeContainer(ctx, existing.ID)
	}

	if e.opts.ComposeBackend == ComposeBackendNative {
		orch := compose.NewOrchestrator(e.runtime)
		if err := orch.Down(ctx, &compose.DownPlan{
			ProjectName: projectName,
		}); err != nil {
			return fmt.Errorf("compose down for recreate (native): %w", err)
		}
		return nil
	}

	cr, ok := e.runtime.(runtime.ComposeRuntime)
	if !ok {
		// Shellout backend selected but runtime doesn't support it;
		// fall back to removing the single container we saw rather
		// than failing the whole Recreate.
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
		opts.bus.Emit(events.BuildStartEvent{Source: events.BuildSourceImage, Ref: s.Image})
		if err := e.ensureImage(ctx, s.Image, opts.PullPolicy, opts.bus.BuildChan(events.BuildSourceImage)); err != nil {
			return "", err
		}
		return s.Image, nil

	case *config.BuildSource:
		tag := "dc-go-base-" + cfg.DevcontainerID + ":latest"
		cacheFrom := s.CacheFrom
		var (
			noCache  bool
			platform string
		)
		if opts.override != nil {
			if opts.override.BaseIsFinal && opts.override.FinalTag != "" {
				tag = opts.override.FinalTag
			}
			noCache = opts.override.NoCache
			platform = opts.override.Platform
			cacheFrom = append(append([]string(nil), cacheFrom...), opts.override.ExtraCacheFrom...)
		}
		opts.bus.Emit(events.BuildStartEvent{Source: events.BuildSourceDockerfile, Ref: tag})
		_, err := e.runtime.BuildImage(ctx, runtime.BuildSpec{
			ContextPath: s.Context,
			Dockerfile:  s.Dockerfile,
			Tag:         tag,
			Args:        s.Args,
			Target:      s.Target,
			CacheFrom:   cacheFrom,
			NoCache:     noCache,
			Platform:    platform,
		}, opts.bus.BuildChan(events.BuildSourceDockerfile))
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
// feature-extending Dockerfile, and builds it. Returns (finalImage,
// baseLayers) — baseLayers is the parsed devcontainer.metadata label
// from the base image (nil if the image carries no label or is
// unreadable), surfaced for the caller's metadata-merge pass.
//
// Even with no features to layer, the base image's label is still read:
// remoteUser / lifecycle hooks / etc. baked into the image must reach
// the merge step. Inspect failures are non-fatal — the caller proceeds
// without baseLayers, matching prior behavior.
//
// Side effect: cfg.Features entries are mutated in place to populate
// Dir, Metadata, ResolvedRef on fetch. Caller's Workspace.Config sees
// the post-fetch state.
func (e *Engine) layerFeatures(ctx context.Context, cfg *config.ResolvedConfig, baseImage string, opts UpOptions) (string, []config.FeatureMetadata, error) {
	// Read the base image's devcontainer.metadata label unconditionally —
	// it carries baked-in config (remoteUser, lifecycle hooks, env, ...)
	// that the merge pipeline must see even when no features are layered.
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

	if len(cfg.Features) == 0 {
		return baseImage, baseMeta, nil
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

	// Emit a Skipped event for features the base-image label satisfied
	// (markAlreadyInstalled has already flipped their AlreadyInstalled
	// flag). Done before the fetch loop so the event order is:
	// skip(s) → resolve_start → resolved → ...
	emitFeatureSkippedFromLabel(opts.bus, cfg.Features)

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
		opts.bus.Emit(events.FeatureResolveStartEvent{Ref: f.Ref})
		fetched, err := e.featureStore.Fetch(ctx, ref, f.SourceKind)
		if err != nil {
			return "", nil, fmt.Errorf("fetch feature %s: %w", f.Ref, err)
		}
		f.Dir = fetched.Dir
		f.ResolvedRef = fetched.ResolvedRef
		f.Metadata = fetched.Metadata
		// FromCache is left false: the current Store interface doesn't
		// distinguish cached from network fetches. See feature.Fetched.
		opts.bus.Emit(events.FeatureResolvedEvent{Ref: f.Ref, Digest: fetched.ResolvedRef})

		// Apply spec defaults + validate against the now-known options.
		merged, mwarns, err := feature.MergeOptions(f.Metadata, f.Options)
		if err != nil {
			return "", nil, fmt.Errorf("feature %s: %w", f.Ref, err)
		}
		f.Options = merged
		cfg.Warnings = append(cfg.Warnings, mwarns...)
	}

	// Re-order with fully-populated metadata so installsAfter / dependsOn apply.
	ordered, oWarns, err := feature.Order(cfg.Features, nil)
	if err != nil {
		return "", nil, err
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
		return baseImage, baseMeta, nil
	}

	tmp, err := os.MkdirTemp("", "dc-go-build-*")
	if err != nil {
		return "", nil, fmt.Errorf("create build context tmpdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := feature.GenerateBuildContext(plan, tmp); err != nil {
		return "", nil, fmt.Errorf("generate feature build context: %w", err)
	}

	finalTag := "dc-go-final-" + cfg.DevcontainerID + ":latest"
	var (
		noCache   bool
		platform  string
		cacheFrom []string
	)
	if opts.override != nil {
		if !opts.override.BaseIsFinal && opts.override.FinalTag != "" {
			finalTag = opts.override.FinalTag
		}
		noCache = opts.override.NoCache
		platform = opts.override.Platform
		cacheFrom = append([]string(nil), opts.override.ExtraCacheFrom...)
	}
	opts.bus.Emit(events.BuildStartEvent{Source: events.BuildSourceFeatures, Ref: finalTag})
	_, err = e.runtime.BuildImage(ctx, runtime.BuildSpec{
		ContextPath: tmp,
		Dockerfile:  "Dockerfile",
		Tag:         finalTag,
		Args: map[string]string{
			"_DEV_CONTAINERS_BASE_IMAGE": baseImage,
		},
		CacheFrom: cacheFrom,
		NoCache:   noCache,
		Platform:  platform,
	}, opts.bus.BuildChan(events.BuildSourceFeatures))
	if err != nil {
		return "", nil, fmt.Errorf("build feature-extended image: %w", err)
	}
	return finalTag, baseMeta, nil
}

// applyMetadataMerge folds the base-image label and per-feature
// devcontainer-feature.json metadata into cfg, then applies spec
// defaults via Finalize. Layers go in spec order: base image first,
// each fetched feature next; the user's devcontainer.json (already in
// cfg) wins on conflicts.
//
// AlreadyInstalled features are NOT re-added — their metadata is
// already part of baseLayers as a prior label entry. (Re-adding is
// harmless for idempotent fields but produces duplicate hooks for
// lifecycle phases.)
func applyMetadataMerge(cfg *config.ResolvedConfig, baseLayers []config.FeatureMetadata, localEnv map[string]string) {
	chain := make([]config.FeatureMetadata, 0, len(baseLayers)+len(cfg.Features))
	chain = append(chain, baseLayers...)
	for _, f := range cfg.Features {
		if f.AlreadyInstalled {
			continue
		}
		chain = append(chain, f.Metadata)
	}
	subCtx := config.SubstitutionContext{
		LocalWorkspaceFolder:     cfg.LocalWorkspaceFolder,
		ContainerWorkspaceFolder: cfg.ContainerWorkspaceFolder,
		DevcontainerID:           cfg.DevcontainerID,
		LocalEnv:                 localEnv,
	}
	config.MergeMetadata(cfg, subCtx, chain)
	cfg.Finalize()
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
		Init:            config.BoolOr(cfg.Init, false),
		Privileged:      config.BoolOr(cfg.Privileged, false),
		CapAdd:          cfg.CapAdd,
		SecurityOpt:     cfg.SecurityOpt,
		OverrideCommand: config.BoolOr(cfg.OverrideCommand, true),
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
