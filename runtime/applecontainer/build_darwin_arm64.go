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

// buildSpecWire mirrors applecontainer-bridge/Sources/ACBridge/build.swift's
// BuildSpecJSON. Engine concepts we don't model on this backend
// (RunArgs/Privileged/SecurityOpt analogs) are intentionally absent
// from this wire type — same pattern as runSpecJSON in lifecycle.
type buildSpecWire struct {
	ContextPath string            `json:"contextPath"`
	Dockerfile  string            `json:"dockerfile,omitempty"`
	Tag         string            `json:"tag,omitempty"`
	Args        map[string]string `json:"args,omitempty"`
	Target      string            `json:"target,omitempty"`
	CacheFrom   []string          `json:"cacheFrom,omitempty"`
	NoCache     bool              `json:"noCache,omitempty"`
	Platform    string            `json:"platform,omitempty"`
}

type buildResultData struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

// BuildImage performs a Dockerfile build via Apple's BuildKit
// container. Requires the user to have run `container builder start`
// beforehand — auto-start would require reimplementing
// BuilderStart.start which is module-internal to ContainerCommands.
//
// Behavior:
//   - If the builder is not running, returns
//     *runtime.BuilderUnavailableError with a hint.
//   - Otherwise dials buildkit over vsock, runs the BuildKit build,
//     loads the resulting OCI tarball into the local content store,
//     tags it with spec.Tag, and returns a runtime.ImageRef.
//
// Progress events: PR-G2 ships without streaming events. BuildKit's
// progress output goes to the bridge's stderr (which the Go process
// inherits). Callers see raw build output on the console. A future
// PR can swap in a pipe-fd capture and emit typed BuildEvents.
func (r *Runtime) BuildImage(ctx context.Context, spec runtime.BuildSpec, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	if err := ctx.Err(); err != nil {
		return runtime.ImageRef{}, err
	}
	if err := ensureLoaded(); err != nil {
		return runtime.ImageRef{}, err
	}

	// Builder-liveness probe. Cheap and gives us a clean typed error
	// before we marshal the full spec. The bridge tags the
	// builder-down case with code="BUILDER_UNAVAILABLE"; we key off
	// that rather than message text.
	probeRaw := goStringAndFree(C.ac_build_probe_p())
	if probeRaw == "" {
		return runtime.ImageRef{}, errors.New("applecontainer: bridge returned nil for BuildImage probe")
	}
	if probeEnv, err := decodeEnvelope[map[string]any](probeRaw); err != nil {
		if probeEnv.Code == bridgeCodeBuilderUnavailable {
			return runtime.ImageRef{}, &runtime.BuilderUnavailableError{
				Hint: "run `container builder start` to start the build VM",
				Err:  err,
			}
		}
		return runtime.ImageRef{}, err
	}

	emitBuildEvent(events, runtime.BuildEvent{
		Kind:    runtime.BuildEventLog,
		Message: "applecontainer: building " + spec.Tag,
	})

	wire := buildSpecWire{
		ContextPath: spec.ContextPath,
		Dockerfile:  spec.Dockerfile,
		Tag:         spec.Tag,
		Args:        spec.Args,
		Target:      spec.Target,
		CacheFrom:   spec.CacheFrom,
		NoCache:     spec.NoCache,
		Platform:    spec.Platform,
	}
	specBytes, err := json.Marshal(wire)
	if err != nil {
		return runtime.ImageRef{}, fmt.Errorf("applecontainer: marshal BuildSpec: %w", err)
	}
	cSpec := C.CString(string(specBytes))
	defer C.free(unsafe.Pointer(cSpec))

	raw := goStringAndFree(C.ac_build_p(cSpec))
	if raw == "" {
		return runtime.ImageRef{}, errors.New("applecontainer: bridge returned nil for BuildImage")
	}
	env, err := decodeEnvelope[buildResultData](raw)
	if err != nil {
		// Apple's "builder not running" error path can surface here
		// if the buildkit container vanished between probe and build.
		// Primary key is the bridge's machine-readable `code`; the
		// message-text fallback covers older bridge builds.
		if env.Code == bridgeCodeBuilderUnavailable || isBuilderUnavailable(err) {
			return runtime.ImageRef{}, &runtime.BuilderUnavailableError{
				Hint: "run `container builder start` to start the build VM",
				Err:  err,
			}
		}
		return runtime.ImageRef{}, err
	}

	emitBuildEvent(events, runtime.BuildEvent{
		Kind:    runtime.BuildEventCompleted,
		Digest:  env.decoded.Digest,
		Message: "applecontainer: built " + env.decoded.Reference,
	})

	tags := []string{}
	if env.decoded.Reference != "" {
		tags = append(tags, env.decoded.Reference)
	}
	return runtime.ImageRef{
		ID:   env.decoded.Digest,
		Tags: tags,
	}, nil
}

// bridgeCodeBuilderUnavailable matches the `code` the Swift bridge
// stamps on the failure envelope when Apple's buildkit container is
// not running. Keep in sync with applecontainer-bridge/Sources/
// ACBridge/build.swift.
const bridgeCodeBuilderUnavailable = "BUILDER_UNAVAILABLE"

func isBuilderUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsAny(msg,
		"builder not running",
		"container buildkit not found",
		"buildkit container",
	)
}
