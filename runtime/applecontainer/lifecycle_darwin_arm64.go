//go:build darwin && arm64

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
	"unsafe"

	"github.com/crunchloop/devcontainer/runtime"
)

// runSpecJSON is the wire shape sent to the bridge. Fields are kept
// in lockstep with applecontainer-bridge/Sources/ACBridge/lifecycle.swift's
// RunSpecJSON. Anything we silently drop on this backend
// (RunArgs, Privileged, SecurityOpt) is intentionally absent here, so
// the build fails if a caller starts depending on those fields without
// updating the design doc.
type runSpecJSON struct {
	Image           string            `json:"image"`
	ID              string            `json:"id"`
	Cmd             []string          `json:"cmd,omitempty"`
	Entrypoint      []string          `json:"entrypoint,omitempty"`
	User            string            `json:"user,omitempty"`
	WorkingDir      string            `json:"workingDir,omitempty"`
	Env             []string          `json:"env,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Mounts          []mountJSON       `json:"mounts,omitempty"`
	Networks        []string          `json:"networks,omitempty"`
	InitProcess     bool              `json:"initProcess,omitempty"`
	CapAdd          []string          `json:"capAdd,omitempty"`
	OverrideCommand bool              `json:"overrideCommand,omitempty"`
}

type mountJSON struct {
	Type     string `json:"type"`
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

type runResultData struct {
	ID string `json:"id"`
}

// RunContainer creates a container from a RunSpec, returning the
// resulting handle. Apple's split is create→bootstrap→start; this
// method only does create, matching runtime.Runtime's contract that
// callers invoke StartContainer separately so the engine can write
// idempotency markers between phases.
func (r *Runtime) RunContainer(ctx context.Context, spec runtime.RunSpec) (*runtime.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLoaded(); err != nil {
		return nil, err
	}
	if err := rejectUnsupportedRunSpec(spec); err != nil {
		return nil, err
	}
	wire := runSpecToWire(spec)
	if wire.ID == "" {
		return nil, errors.New("applecontainer: RunSpec.Name is required (used as container id)")
	}
	if wire.Image == "" {
		return nil, errors.New("applecontainer: RunSpec.Image is required")
	}
	specBytes, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("applecontainer: marshal RunSpec: %w", err)
	}
	cSpec := C.CString(string(specBytes))
	defer C.free(unsafe.Pointer(cSpec))

	raw := goStringAndFree(C.ac_run_p(cSpec))
	if raw == "" {
		return nil, errors.New("applecontainer: bridge returned nil for RunContainer")
	}
	env, err := decodeEnvelope[runResultData](raw)
	if err != nil {
		return nil, mapRunErr(spec.Image, err)
	}
	return &runtime.Container{
		ID:    env.decoded.ID,
		Name:  env.decoded.ID,
		Image: spec.Image,
		State: runtime.StateCreated,
	}, nil
}

// StartContainer bootstraps and starts a previously created container.
// Idempotent against already-running containers (the bridge short-
// circuits when the snapshot reports running).
func (r *Runtime) StartContainer(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureLoaded(); err != nil {
		return err
	}
	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))
	raw := goStringAndFree(C.ac_start_p(cID))
	if raw == "" {
		return errors.New("applecontainer: bridge returned nil for StartContainer")
	}
	if _, err := decodeEnvelope[json.RawMessage](raw); err != nil {
		return mapLifecycleErr(id, err)
	}
	return nil
}

// StopContainer sends SIGTERM with a grace period, then SIGKILL. Apple's
// stop API takes an Int32 grace period in seconds; we round nanos up
// and clamp.
func (r *Runtime) StopContainer(ctx context.Context, id string, opts runtime.StopOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureLoaded(); err != nil {
		return err
	}
	var graceSec int32
	if opts.Timeout > 0 {
		// Round up to whole seconds, clamp to MaxInt32 in the unlikely
		// case of a wildly-large timeout.
		secs := (opts.Timeout.Nanoseconds() + 999_999_999) / 1_000_000_000
		if secs > 1<<31-1 {
			secs = 1<<31 - 1
		}
		graceSec = int32(secs)
	}
	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))
	raw := goStringAndFree(C.ac_stop_p(cID, C.int32_t(graceSec)))
	if raw == "" {
		return errors.New("applecontainer: bridge returned nil for StopContainer")
	}
	if _, err := decodeEnvelope[json.RawMessage](raw); err != nil {
		return mapLifecycleErr(id, err)
	}
	return nil
}

// RemoveContainer deletes a container. Force=true allows deletion of
// running containers; RemoveVolumes is silently dropped on this
// backend (volume lifecycle isn't yet wired up — see PR-C scope cuts
// in design §8).
func (r *Runtime) RemoveContainer(ctx context.Context, id string, opts runtime.RemoveOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureLoaded(); err != nil {
		return err
	}
	var force C.int32_t
	if opts.Force {
		force = 1
	}
	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))
	raw := goStringAndFree(C.ac_delete_p(cID, force))
	if raw == "" {
		return errors.New("applecontainer: bridge returned nil for RemoveContainer")
	}
	if _, err := decodeEnvelope[json.RawMessage](raw); err != nil {
		return mapLifecycleErr(id, err)
	}
	return nil
}

// rejectUnsupportedRunSpec fails fast on RunSpec fields that this
// backend cannot honor. Documented in design §8 as not modeled on
// Apple's `container` runtime; rather than silently dropping them
// (which would let callers observe apparent success with the option
// ignored), we surface a typed UnsupportedOptionError before crossing
// the cgo boundary.
func rejectUnsupportedRunSpec(spec runtime.RunSpec) error {
	if len(spec.RunArgs) > 0 {
		return &runtime.UnsupportedOptionError{Backend: "applecontainer", Option: "RunArgs"}
	}
	if spec.Privileged {
		return &runtime.UnsupportedOptionError{Backend: "applecontainer", Option: "Privileged"}
	}
	if len(spec.SecurityOpt) > 0 {
		return &runtime.UnsupportedOptionError{Backend: "applecontainer", Option: "SecurityOpt"}
	}
	return nil
}

// runSpecToWire projects runtime.RunSpec onto the JSON shape the
// bridge expects. Fields the apple-container backend does not support
// (RunArgs, Privileged, SecurityOpt) are rejected by
// rejectUnsupportedRunSpec before reaching this function, so they
// don't appear in the wire type at all.
func runSpecToWire(spec runtime.RunSpec) runSpecJSON {
	out := runSpecJSON{
		Image:           spec.Image,
		ID:              spec.Name,
		Cmd:             spec.Cmd,
		Entrypoint:      spec.Entrypoint,
		User:            spec.User,
		WorkingDir:      spec.WorkingDir,
		Env:             envMapToSlice(spec.Env),
		Labels:          spec.Labels,
		Mounts:          mapMounts(spec.Mounts),
		Networks:        append([]string(nil), spec.Networks...),
		InitProcess:     spec.Init,
		CapAdd:          spec.CapAdd,
		OverrideCommand: spec.OverrideCommand,
	}
	return out
}

func envMapToSlice(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func mapMounts(in []runtime.MountSpec) []mountJSON {
	if len(in) == 0 {
		return nil
	}
	out := make([]mountJSON, 0, len(in))
	for _, m := range in {
		out = append(out, mountJSON{
			Type:     mountTypeToWire(m.Type),
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

func mountTypeToWire(t runtime.MountType) string {
	switch t {
	case runtime.MountBind:
		return "bind"
	case runtime.MountTmpfs:
		return "tmpfs"
	case runtime.MountVolume:
		return "volume"
	default:
		return string(t)
	}
}

func mapRunErr(image string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// Apple emits `notFound: "image with reference <ref>"` from
	// ClientImage.get; less-specific paths might surface "no such
	// image" / "no metadata for image" / "image not found". Match
	// loosely so wording shifts don't silently drop the typed-error
	// contract.
	if containsAny(msg, "image with reference", "image not found", "no such image", "no metadata for image") {
		return &runtime.ImageNotFoundError{Ref: image, Err: err}
	}
	return err
}

func mapLifecycleErr(id string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if containsAny(msg, "notFound", "not found") {
		return &runtime.ContainerNotFoundError{ID: id, Err: err}
	}
	return err
}
