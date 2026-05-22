package devcontainer

import (
	"context"
	"fmt"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
)

// BuildOptions configures Engine.Build.
type BuildOptions struct {
	// LocalWorkspaceFolder is the absolute host path to the project. Required.
	LocalWorkspaceFolder string

	// ConfigPath is the absolute path to devcontainer.json. If empty,
	// discovered under LocalWorkspaceFolder per Resolve's rules.
	ConfigPath string

	// LocalEnv overrides os.Environ() for ${localEnv:*} resolution.
	// Nil means use the current process environment.
	LocalEnv map[string]string

	// ImageName, when non-empty, is the tag applied to the final
	// produced image. It replaces the engine's auto-generated tag
	// (dc-go-final-<id>:latest or dc-go-base-<id>:latest). Ignored for
	// pure image sources with no features layered (there is nothing
	// to retag — the engine would need a TagImage primitive it
	// doesn't currently expose; tracked as a follow-up).
	ImageName string

	// NoCache forces --no-cache on every BuildImage call in the chain
	// (Dockerfile build, features build).
	NoCache bool

	// PullPolicy controls when the base image is pulled. Default
	// IfNotPresent — same as Up.
	PullPolicy PullPolicy

	// Platform pins the target platform (e.g. "linux/amd64",
	// "linux/arm64") on every BuildImage call. Empty leaves it
	// unspecified.
	Platform string

	// CacheFrom is appended to whatever the source declares
	// (devcontainer.json `build.cacheFrom`). Empty means no extra
	// cache sources.
	CacheFrom []string

	// Events optionally receives structured engine events for the
	// duration of this Build call: ConfigResolved, ConfigWarning,
	// BuildStart/Log/Layer/Completed, FeatureResolveStart, FeatureResolved.
	// Drop-on-full; the engine never blocks on send. See package events
	// (experimental until v1.0.0).
	//
	// Ownership: the caller owns the channel. The engine only writes —
	// it never closes the channel. The caller MUST NOT close it before
	// Build returns.
	Events chan<- events.Event
}

// BuildResult is the outcome of Engine.Build.
type BuildResult struct {
	// ImageID is the tag of the produced image. When BuildOptions.ImageName
	// was set and applicable, it equals that. Otherwise it's the engine's
	// auto-generated tag (dc-go-final-<id>:latest or dc-go-base-<id>:latest).
	ImageID string
}

// Build resolves a workspace's devcontainer.json and produces the final
// container image (base image plus the feature pipeline) without
// creating or running a container.
//
// Compose sources are refused with a typed error — use the compose
// workflow for those.
//
// updateRemoteUserUID is deliberately skipped: Build's output is
// designed to be portable (pushed to a registry, reused across hosts),
// while UID reconciliation bakes the calling host's UID into the
// image. Use Engine.Up if you need a UID-reconciled local image.
//
// For pure image sources with no features, Build short-circuits: it
// just ensures the image is present locally and returns its ref.
// BuildOptions.ImageName is ignored in that case (no build step to
// retag); a TagImage primitive is tracked as a follow-up.
func (e *Engine) Build(ctx context.Context, opts BuildOptions) (*BuildResult, error) {
	if err := ctxIfDone(ctx); err != nil {
		return nil, err
	}
	if opts.LocalWorkspaceFolder == "" {
		return nil, fmt.Errorf("BuildOptions.LocalWorkspaceFolder is required")
	}

	bus := newEventBus(e.emitter, opts.Events)
	defer bus.Close()

	cfg, err := Resolve(ctx, ResolveOptions{
		LocalWorkspaceFolder: opts.LocalWorkspaceFolder,
		ConfigPath:           opts.ConfigPath,
		LocalEnv:             opts.LocalEnv,
	})
	if err != nil {
		return nil, err
	}
	bus.Emit(events.ConfigResolvedEvent{Config: cfg})
	for _, w := range cfg.Warnings {
		bus.Emit(events.ConfigWarningEvent{Code: string(w.Code), Message: w.Message})
	}

	if _, isCompose := cfg.Source.(*config.ComposeSource); isCompose {
		return nil, fmt.Errorf("Engine.Build: compose-source devcontainers are not supported")
	}

	_, isBuildSource := cfg.Source.(*config.BuildSource)
	baseIsFinal := isBuildSource && len(cfg.Features) == 0

	upOpts := UpOptions{
		LocalWorkspaceFolder: opts.LocalWorkspaceFolder,
		ConfigPath:           opts.ConfigPath,
		LocalEnv:             opts.LocalEnv,
		PullPolicy:           opts.PullPolicy,
		bus:                  bus,
		override: &buildOverride{
			FinalTag:       opts.ImageName,
			BaseIsFinal:    baseIsFinal,
			NoCache:        opts.NoCache,
			Platform:       opts.Platform,
			ExtraCacheFrom: opts.CacheFrom,
		},
	}

	baseImage, err := e.prepareBaseImage(ctx, cfg, upOpts)
	if err != nil {
		return nil, err
	}

	finalImage, _, err := e.layerFeatures(ctx, cfg, baseImage, upOpts)
	if err != nil {
		return nil, err
	}

	return &BuildResult{ImageID: finalImage}, nil
}
