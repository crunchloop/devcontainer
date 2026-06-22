// Package podman implements runtime.Runtime on Podman, adding
// CRIU-backed checkpoint/restore (runtime.CheckpointRuntime) — the one
// engine that does the full migration round trip (docker's restore is
// broken on current versions; see design/checkpoint-restore.md).
//
// Transport (design/podman-backend.md, Option A): the standard Runtime
// surface (run/exec/inspect/pull/networks/…) is served by an embedded
// *docker.Runtime pointed at Podman's docker-compatible socket — Podman
// exposes the moby REST API there, so the existing, well-tested docker
// backend works unchanged. Two areas differ and are overridden here,
// both driven through the libpod REST API on the SAME socket (a thin
// stdlib HTTP client — no `podman` CLI subprocess, no heavy
// pkg/bindings dependency):
//
//   - Checkpoint/Restore: libpod-only, not in the docker-compat API.
//   - BuildImage: the docker backend's build is BuildKit-only, which
//     Podman's docker-compat /build does not provide; we build with
//     buildah via the libpod /build endpoint.
package podman

import (
	"context"
	"fmt"

	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

// Compile-time assertions: *Runtime satisfies the core Runtime interface
// and the optional CheckpointRuntime sub-interface.
var (
	_ runtime.Runtime           = (*Runtime)(nil)
	_ runtime.CheckpointRuntime = (*Runtime)(nil)
)

// Runtime is the Podman backend. It embeds a *docker.Runtime (wired to
// Podman's docker-compatible socket) for the standard surface and adds
// the libpod-only checkpoint/restore + buildah build via a thin libpod
// HTTP client over the same socket.
type Runtime struct {
	*docker.Runtime

	lp *libpodClient

	// checkpointOK gates Capabilities().Checkpoint: the libpod API was
	// reachable at New, and Options.CheckpointProbe (if supplied) returned
	// true. See Options.CheckpointProbe for why CRIU itself can't be
	// probed over the socket.
	checkpointOK bool
}

// Options configure New.
type Options struct {
	// Socket is the Podman service socket serving both the
	// docker-compatible and libpod APIs (e.g.
	// "unix:///run/podman/podman.sock"). Required — Podman must be
	// running `podman system service`.
	Socket string

	// CheckpointProbe optionally asserts CRIU availability on the host
	// serving Socket. It gates Capabilities().Checkpoint together with
	// libpod reachability, runs once at New, and its result is cached.
	//
	// The backend cannot verify CRIU itself: the libpod REST API has no
	// `criu check` equivalent and /info doesn't report CRIU, and the
	// backend is deliberately CLI-free (no `criu check` shell-out). But
	// the deployer runs `podman system service` and knows the host, so
	// they can supply a probe (exec `criu check`, read a provisioning
	// marker, etc.).
	//
	// Nil means "don't probe": Capabilities().Checkpoint then reflects
	// libpod reachability only, and a missing CRIU surfaces at Checkpoint
	// time as a *runtime.CheckpointFailedError (callers fall back to a
	// cold Up — workspace data on the volume is intact).
	CheckpointProbe func(context.Context) bool
}

// New constructs a Podman runtime: wires the embedded docker.Runtime to
// the Podman service socket and a libpod client to the same socket.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	dr, err := docker.New(ctx, docker.Options{Host: opts.Socket})
	if err != nil {
		return nil, fmt.Errorf("podman: connect to service socket %q: %w", opts.Socket, err)
	}
	lp := newLibpodClient(opts.Socket)
	return &Runtime{
		Runtime:      dr,
		lp:           lp,
		checkpointOK: probeCheckpoint(ctx, lp, opts.CheckpointProbe),
	}, nil
}

// probeCheckpoint reports whether the Checkpoint capability should be set:
// the libpod API must be reachable (a 2xx from /libpod/_ping, which also
// confirms it is genuinely Podman — a docker socket 404s the /libpod/
// path), and any caller-supplied CRIU probe must also pass. Split out of
// New so it is unit-testable without a daemon.
func probeCheckpoint(ctx context.Context, lp *libpodClient, probe func(context.Context) bool) bool {
	if !lp.ping(ctx) {
		return false
	}
	if probe != nil {
		return probe(ctx)
	}
	return true
}

// Capabilities reports the Podman backend's feature profile. It does not
// delegate to the embedded docker.Runtime: Podman has its own profile,
// and Checkpoint is the bit that matters here.
func (r *Runtime) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		Healthchecks:     true,
		ExitCodes:        true,
		NamespaceSharing: true,
		RestartPolicies:  true,
		SharedVolumes:    true,
		ServiceNameDNS:   true,
		Checkpoint:       r.checkpointOK,
	}
}
