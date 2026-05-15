//go:build darwin && arm64

// libACBridge.dylib is loaded at runtime via dlopen — see
// embed_darwin_arm64.go for the embed-and-extract mechanic. This file
// only links the cgo C shim (shim.c / shim.h) that wraps dlsym'd
// function pointers. No build-time dependency on the SwiftPM output
// directory; the dylib travels embedded in the binary.
package applecontainer

/*
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
	"unsafe"

	"github.com/crunchloop/devcontainer/runtime"
)

// Runtime is the apple-container implementation of runtime.Runtime.
//
// PR-A surface: New + Ping. All other methods return
// runtime.ErrNotImplemented until PR-B onward.
type Runtime struct {
	bridgeVersion string
}

// Compile-time assertion: *Runtime satisfies runtime.Runtime. Renaming
// or adding interface methods upstream breaks the build here, surfacing
// the gap immediately.
var _ runtime.Runtime = (*Runtime)(nil)

// Options configure New.
type Options struct {
	// PingTimeoutSeconds bounds the daemon-health probe in New. Zero
	// uses the bridge default (5s).
	PingTimeoutSeconds int
}

// PingResult is the parsed result of a daemon health-check probe.
type PingResult struct {
	APIServerVersion string `json:"apiServerVersion"`
	APIServerBuild   string `json:"apiServerBuild"`
	APIServerCommit  string `json:"apiServerCommit"`
	AppRoot          string `json:"appRoot"`
	InstallRoot      string `json:"installRoot"`
}

// New constructs an apple-container runtime. The constructor extracts
// the embedded bridge dylib (idempotent, hashed cache file), dlopens
// it, then probes the daemon via ClientHealthCheck.ping. Returns a
// *runtime.DaemonUnavailableError if the daemon is not reachable.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLoaded(); err != nil {
		return nil, err
	}
	r := &Runtime{bridgeVersion: bridgeVersion()}
	if _, err := r.Ping(ctx, opts.PingTimeoutSeconds); err != nil {
		return nil, err
	}
	return r, nil
}

// Ping probes the daemon. Returns a *runtime.DaemonUnavailableError if
// the apiserver is unreachable (daemon not started, version skew, EUID
// mismatch). The timeoutSeconds argument bounds the underlying Swift
// `ClientHealthCheck.ping` call; <=0 uses the bridge default (5s).
//
// Exposed as a method (not just an internal helper) so callers can
// re-probe a live runtime — useful for long-running consumers that
// want to detect a daemon restart.
func (r *Runtime) Ping(ctx context.Context, timeoutSeconds int) (*PingResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLoaded(); err != nil {
		return nil, err
	}
	// Respect ctx.Deadline() by clamping timeoutSeconds to the
	// remaining time. The bridge call itself is synchronous from Go's
	// perspective, so we can't cancel it mid-flight — bounding the
	// argument is the next-best contract.
	effective := timeoutSeconds
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ctx.Err()
		}
		deadlineSec := int(math.Ceil(remaining.Seconds()))
		if effective <= 0 || deadlineSec < effective {
			effective = deadlineSec
		}
	}
	cstr := C.ac_ping_p(C.int32_t(effective))
	if cstr == nil {
		return nil, &runtime.DaemonUnavailableError{Err: errors.New("bridge returned nil")}
	}
	raw := C.GoString(cstr)
	C.ac_free_p(unsafe.Pointer(cstr))

	var payload struct {
		OK  bool   `json:"ok"`
		Err string `json:"err"`
		PingResult
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("applecontainer: bridge returned invalid ping response %q: %w", raw, err)
	}
	if !payload.OK {
		return nil, &runtime.DaemonUnavailableError{Err: errors.New(payload.Err)}
	}
	return &payload.PingResult, nil
}

// BridgeVersion returns the version string baked into libACBridge.dylib.
// Useful for diagnostics and confirming the linked bridge matches what
// the test suite expects.
func (r *Runtime) BridgeVersion() string {
	return r.bridgeVersion
}

func bridgeVersion() string {
	cstr := C.ac_version_p()
	if cstr == nil {
		return ""
	}
	defer C.ac_free_p(unsafe.Pointer(cstr))
	return C.GoString(cstr)
}

// ---- runtime.Runtime stubs (filled in by later PRs) ------------------

func (*Runtime) BuildImage(context.Context, runtime.BuildSpec, chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	return runtime.ImageRef{}, runtime.ErrNotImplemented
}

// InspectContainer, InspectImage, FindContainerByLabel — PR-B
//   (inspect_darwin_arm64.go).
// RunContainer, StartContainer, StopContainer, RemoveContainer — PR-C
//   (lifecycle_darwin_arm64.go).
// ExecContainer — PR-D (exec_darwin_arm64.go).
// ContainerLogs — PR-E (logs_darwin_arm64.go).
// PullImage — PR-F (pull_darwin_arm64.go).

