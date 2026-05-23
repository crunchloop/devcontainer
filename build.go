package devcontainer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/runtime"
)

// ErrComposeSourceUnsupported is returned (wrapped) from Engine.Build
// when the resolved devcontainer.json is a compose source. Callers can
// match with errors.Is to surface a friendlier message or fall back to
// the compose workflow.
var ErrComposeSourceUnsupported = errors.New("compose-source devcontainers are not supported by Engine.Build")

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
	// (dc-go-final-<id>:latest or dc-go-base-<id>:latest).
	//
	// Honored uniformly across source kinds. For paths where no build
	// step would otherwise carry the tag — pure image source, or a
	// build source whose features are all already pre-baked into the
	// base image — Build runs a thin `FROM <baseImage>` Dockerfile
	// build to apply the tag. (This workaround goes away once the
	// runtime exposes a TagImage primitive; tracked as a follow-up.)
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
// When the feature pipeline produces no work (no features declared,
// or every requested feature is already present in the base image's
// metadata label), Build still honors BuildOptions.ImageName via a
// thin no-op `FROM <baseImage>` Dockerfile build that applies the
// tag.
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
		return nil, fmt.Errorf("Engine.Build: %w", ErrComposeSourceUnsupported)
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

	// If the feature pipeline returned baseImage unchanged (no features
	// declared, or every feature was already pre-baked into the base
	// image's metadata) and the caller asked for a specific tag, apply
	// it via a thin `FROM <baseImage>` build. Until the runtime grows a
	// TagImage primitive this is the only way to honor ImageName in
	// these short-circuit paths.
	if opts.ImageName != "" && finalImage == baseImage && finalImage != opts.ImageName {
		finalImage, err = e.retagImage(ctx, baseImage, opts.ImageName, upOpts.override, bus)
		if err != nil {
			return nil, err
		}
	}

	return &BuildResult{ImageID: finalImage}, nil
}

// retagImage applies `target` as an additional tag on `source` by
// running a one-line `FROM <source>` Dockerfile build. Used by Build
// when the feature pipeline short-circuits and the caller asked for
// a specific ImageName — there's no other tag-affecting step in the
// pipeline to carry the tag, and the runtime doesn't expose a
// dedicated tag primitive yet.
//
// Cost: roughly one BuildKit FROM-fetch + a metadata write. No
// userland layers are added.
func (e *Engine) retagImage(ctx context.Context, source, target string, override *buildOverride, bus *eventBus) (string, error) {
	tmp, err := os.MkdirTemp("", "dc-go-retag-*")
	if err != nil {
		return "", fmt.Errorf("retag: create tmpdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := os.WriteFile(filepath.Join(tmp, "Dockerfile"), []byte("FROM "+source+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("retag: write Dockerfile: %w", err)
	}

	var (
		noCache   bool
		platform  string
		cacheFrom []string
	)
	if override != nil {
		noCache = override.NoCache
		platform = override.Platform
		cacheFrom = override.ExtraCacheFrom
	}
	bus.Emit(events.BuildStartEvent{Source: events.BuildSourceDockerfile, Ref: target})
	if _, err := e.runtime.BuildImage(ctx, runtime.BuildSpec{
		ContextPath: tmp,
		Dockerfile:  "Dockerfile",
		Tag:         target,
		Platform:    platform,
		CacheFrom:   cacheFrom,
		NoCache:     noCache,
	}, bus.BuildChan(events.BuildSourceDockerfile)); err != nil {
		return "", fmt.Errorf("retag %s as %s: %w", source, target, err)
	}
	return target, nil
}
