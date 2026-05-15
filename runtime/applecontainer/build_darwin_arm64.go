//go:build darwin && arm64

package applecontainer

/*
#include "shim.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"

	"github.com/crunchloop/devcontainer/runtime"
)

// BuildImage on the apple-container backend is partially implemented
// for M6 v0.1:
//
//   - The "builder not running" failure mode is detected and surfaced
//     as a typed *runtime.BuilderUnavailableError so callers can
//     prompt the user with `container builder start`.
//   - The actual Dockerfile build path is intentionally NOT
//     implemented in this PR. Apple's BuildKit-over-vsock integration
//     (dial buildkit container → Builder(socket:) → BuildConfig with
//     ~17 fields → SwiftNIO event loop → progress event translation)
//     is substantial enough to warrant its own focused PR. Callers
//     attempting a build receive a clear "not implemented" error
//     rather than a faked success.
//
// Engine impact: the docker runtime supports build natively; this
// backend doesn't yet, so consumers steering toward apple-container
// must currently use `image:` source devcontainers (which only need
// PullImage from PR-F). Dockerfile + features paths require this
// PR's follow-up.
func (r *Runtime) BuildImage(ctx context.Context, spec runtime.BuildSpec, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	if err := ctx.Err(); err != nil {
		return runtime.ImageRef{}, err
	}
	if err := ensureLoaded(); err != nil {
		return runtime.ImageRef{}, err
	}

	// Probe the builder. If it isn't running, surface a typed error
	// so callers can prompt the user (DAP, an interactive CLI) with
	// the right remediation.
	probeRaw := goStringAndFree(C.ac_build_probe_p())
	if probeRaw == "" {
		return runtime.ImageRef{}, errors.New("applecontainer: bridge returned nil for BuildImage probe")
	}
	if _, err := decodeEnvelope[map[string]any](probeRaw); err != nil {
		return runtime.ImageRef{}, &runtime.BuilderUnavailableError{
			Hint: "run `container builder start` to start the build VM",
			Err:  err,
		}
	}

	// Builder is up, but the actual build path isn't wired through
	// yet. Return a clear, actionable error rather than ErrNotImplemented
	// so callers don't conflate "this backend permanently lacks
	// builds" with "this backend's build is still landing."
	return runtime.ImageRef{}, fmt.Errorf(
		"applecontainer: Dockerfile builds are not yet implemented on this backend (image=%q tag=%q); use a pre-built `image:` source devcontainer",
		spec.Dockerfile, spec.Tag,
	)
}
