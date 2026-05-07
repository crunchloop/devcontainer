package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"

	"github.com/crunchloop/devcontainer/runtime"
)

// Runtime is the Docker Engine implementation of runtime.Runtime
// (and runtime.ComposeRuntime).
type Runtime struct {
	api *client.Client

	composeState composeState
}

// Compile-time assertions: *Runtime satisfies the core Runtime
// interface and the optional ComposeRuntime sub-interface. Missing or
// mismatched signatures fail the build.
var (
	_ runtime.Runtime        = (*Runtime)(nil)
	_ runtime.ComposeRuntime = (*Runtime)(nil)
)

// Options configure New. The zero value is valid: it builds a client
// that reads DOCKER_HOST/DOCKER_API_VERSION/etc. from the environment
// and negotiates the API version with the daemon.
type Options struct {
	// Host overrides DOCKER_HOST (e.g. "unix:///var/run/docker.sock"
	// or "tcp://192.168.0.1:2375"). Empty falls back to env.
	Host string
}

// New constructs a Docker runtime. If the daemon is unreachable the
// returned error is a *runtime.DaemonUnavailableError.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	clientOpts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if opts.Host != "" {
		clientOpts = append(clientOpts, client.WithHost(opts.Host))
	}
	api, err := client.NewClientWithOpts(clientOpts...)
	if err != nil {
		return nil, &runtime.DaemonUnavailableError{Err: err}
	}
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		_ = api.Close()
		return nil, &runtime.DaemonUnavailableError{Err: fmt.Errorf("ping: %w", err)}
	}
	return &Runtime{api: api}, nil
}

// Close releases the underlying HTTP client. Safe to call multiple times.
func (r *Runtime) Close() error {
	if r.api == nil {
		return nil
	}
	return r.api.Close()
}
