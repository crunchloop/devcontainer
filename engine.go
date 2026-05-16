package devcontainer

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/feature"
	"github.com/crunchloop/devcontainer/runtime"
)

// Engine drives the devcontainer lifecycle on top of a Runtime.
type Engine struct {
	runtime      runtime.Runtime
	featureStore feature.Store
	emitter      *events.Emitter
	opts         EngineOptions
}

// EngineOptions configures a new Engine.
type EngineOptions struct {
	// Runtime is the container backend. Required.
	Runtime runtime.Runtime

	// FeatureStore overrides the default feature store. Default:
	// feature.NewDiskStore with the cache and auth options below. Tests
	// substitute this with an in-memory store; production callers
	// usually leave it nil and configure via FeatureCacheDir / OCIKeychain
	// instead.
	FeatureStore feature.Store

	// FeatureCacheDir overrides the default OCI / HTTPS feature cache
	// location (os.UserCacheDir()/devcontainer-go/features). Ignored if
	// FeatureStore is set explicitly.
	FeatureCacheDir string

	// OCIKeychain provides credentials for OCI feature pulls. Nil falls
	// back to authn.DefaultKeychain (ambient docker config / env vars /
	// credential helpers). Callers with short-lived registry tokens
	// (e.g. ECR via STS, GCR via metadata-server) supply a custom
	// Keychain that returns fresh credentials per call. Ignored if
	// FeatureStore is set explicitly.
	OCIKeychain authn.Keychain

	// FeatureDownloadHeaders are additional headers to send on HTTPS
	// feature fetches. Ignored if FeatureStore is set explicitly.
	FeatureDownloadHeaders map[string]string

	// FeatureHTTPSClient overrides the default *http.Client for HTTPS
	// feature fetches. Tests use this to drive httptest servers.
	// Ignored if FeatureStore is set explicitly.
	FeatureHTTPSClient *http.Client

	// StrictFeatureVersionMatch controls how the engine decides whether
	// a feature recorded in a base image's devcontainer.metadata label
	// satisfies the request. Default false (permissive: id match plus
	// baked semver >= requested). True requires byte-level equality on
	// the resolved digest — for reproducible builds. See
	// design/features.md §10.3.
	StrictFeatureVersionMatch bool

	// HostExecutor enables host-side spec hooks (initializeCommand,
	// future secretsCommand). Nil means host hooks return a
	// *LifecycleError wrapping ErrHostExecutorNotConfigured, since
	// host execution is opt-in and security-sensitive — see
	// HostExecutor docs.
	HostExecutor HostExecutor

	// ComposeBackend selects how compose-source devcontainers are
	// brought up. ComposeBackendShellout (default) uses the legacy
	// runtime.ComposeRuntime sub-interface, which on docker shells
	// out to the `docker compose` v2 plugin. ComposeBackendNative
	// uses the runtime-agnostic compose.Orchestrator under compose/
	// driving runtime.Runtime primitives directly (no shellout, no
	// compose plugin dependency, works on every backend that
	// satisfies the §4 primitive surface).
	//
	// See design/compose-native.md §10 for the rollout schedule:
	// Shellout stays default until a confirmed-green release on
	// Native; then the default flips and the shellout path is
	// deleted.
	ComposeBackend ComposeBackend
}

// ComposeBackend selects between the legacy shellout and the new
// runtime-agnostic native orchestrator for compose-source projects.
type ComposeBackend int

const (
	// ComposeBackendShellout (default) uses runtime.ComposeRuntime
	// — `docker compose` v2 plugin under the hood. Reliable for
	// Docker, refused-with-typed-error for backends that don't
	// implement the sub-interface (i.e. applecontainer).
	ComposeBackendShellout ComposeBackend = 0

	// ComposeBackendNative uses compose.Orchestrator driving
	// runtime.Runtime primitives. Backend-agnostic; requires the
	// backend to implement CreateNetwork / CreateVolume /
	// ListContainers / ListImages / RemoveImage / RemoveNetwork /
	// RemoveVolume.
	ComposeBackendNative ComposeBackend = 1
)

// New constructs an Engine. Returns an error if Runtime is nil or the
// feature store cannot be built.
func New(opts EngineOptions) (*Engine, error) {
	if opts.Runtime == nil {
		return nil, errors.New("EngineOptions.Runtime is required")
	}

	store := opts.FeatureStore
	if store == nil {
		ds, err := feature.NewDiskStore(feature.DiskStoreOptions{
			CacheDir:     opts.FeatureCacheDir,
			OCIKeychain:  opts.OCIKeychain,
			HTTPSHeaders: opts.FeatureDownloadHeaders,
			HTTPSClient:  opts.FeatureHTTPSClient,
		})
		if err != nil {
			return nil, fmt.Errorf("feature store: %w", err)
		}
		store = ds
	}

	return &Engine{
		runtime:      opts.Runtime,
		featureStore: store,
		emitter:      events.NewEmitter(nil),
		opts:         opts,
	}, nil
}

// Common labels written to every container the engine creates. Labels are
// the source of truth for container ↔ workspace mapping; container names
// are deterministic but not relied upon for lookup.
const (
	LabelDevcontainerID       = "dev.containers.id"
	LabelLocalWorkspaceFolder = "dev.containers.localWorkspaceFolder"
	LabelConfigPath           = "dev.containers.configPath"
	LabelEngine               = "dev.containers.engine"

	engineIdent = "devcontainer-go/0.1"
)

// containerName returns the deterministic container name for a workspace id.
func containerName(id WorkspaceID) string {
	return "devcontainer-" + string(id)
}

// ctxIfDone returns ctx.Err() if ctx is cancelled, nil otherwise. Used at
// the entry of every public Engine method so that a cancelled ctx never
// triggers a daemon round-trip.
func ctxIfDone(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
