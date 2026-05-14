//go:build darwin && arm64

// PR-A links the Swift bridge at build time via cgo LDFLAGS pointing at
// applecontainer-bridge/.build/.../release. Run `make bridge` before
// `go build ./runtime/applecontainer/...` (or any consumer). The
// embed-and-dlopen distribution path decided in
// design/runtime-applecontainer.md §13.4 lands in a follow-up PR.
package applecontainer

/*
#cgo CFLAGS: -I${SRCDIR}/../../applecontainer-bridge/include
#cgo LDFLAGS: -L${SRCDIR}/../../applecontainer-bridge/.build/arm64-apple-macosx/release -lACBridge -Wl,-rpath,${SRCDIR}/../../applecontainer-bridge/.build/arm64-apple-macosx/release

#include <stdlib.h>
#include "ac_bridge.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// New constructs an apple-container runtime. The constructor probes
// the daemon via ClientHealthCheck.ping and returns a
// *runtime.DaemonUnavailableError if the daemon is not reachable.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if err := ctx.Err(); err != nil {
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
	cstr := C.ac_ping(C.int32_t(timeoutSeconds))
	if cstr == nil {
		return nil, &runtime.DaemonUnavailableError{Err: errors.New("bridge returned nil")}
	}
	raw := C.GoString(cstr)
	C.ac_free(unsafe.Pointer(cstr))

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
	cstr := C.ac_version()
	if cstr == nil {
		return ""
	}
	defer C.ac_free(unsafe.Pointer(cstr))
	return C.GoString(cstr)
}

// ---- runtime.Runtime stubs (filled in PR-B onward) -------------------

func (*Runtime) BuildImage(context.Context, runtime.BuildSpec, chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	return runtime.ImageRef{}, runtime.ErrNotImplemented
}

func (*Runtime) PullImage(context.Context, string, chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	return runtime.ImageRef{}, runtime.ErrNotImplemented
}

func (*Runtime) RunContainer(context.Context, runtime.RunSpec) (*runtime.Container, error) {
	return nil, runtime.ErrNotImplemented
}

func (*Runtime) StartContainer(context.Context, string) error {
	return runtime.ErrNotImplemented
}

func (*Runtime) StopContainer(context.Context, string, runtime.StopOptions) error {
	return runtime.ErrNotImplemented
}

func (*Runtime) RemoveContainer(context.Context, string, runtime.RemoveOptions) error {
	return runtime.ErrNotImplemented
}

func (*Runtime) ExecContainer(context.Context, string, runtime.ExecOptions) (runtime.ExecResult, error) {
	return runtime.ExecResult{}, runtime.ErrNotImplemented
}

func (*Runtime) InspectContainer(context.Context, string) (*runtime.ContainerDetails, error) {
	return nil, runtime.ErrNotImplemented
}

func (*Runtime) InspectImage(context.Context, string) (*runtime.ImageDetails, error) {
	return nil, runtime.ErrNotImplemented
}

func (*Runtime) ContainerLogs(context.Context, string, io.Writer, bool) error {
	return runtime.ErrNotImplemented
}

func (*Runtime) FindContainerByLabel(context.Context, string, string) (*runtime.Container, error) {
	return nil, runtime.ErrNotImplemented
}
