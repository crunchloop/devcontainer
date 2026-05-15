//go:build darwin && arm64

package applecontainer

/*
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"context"
	"errors"
	"unsafe"

	"github.com/crunchloop/devcontainer/runtime"
)

type pullResultData struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

// PullImage fetches an image from a remote registry. PR-F scope:
// synchronous pull, no in-flight progress events. The runtime.Runtime
// contract accepts a BuildEvent channel; we emit a "pulling" log
// event before the call and a "completed" event after, both
// non-blocking. Fine-grained progress is a future PR.
//
// Cancellation: ctx is checked at entry. Once the bridge call is
// in-flight, ctx cancellation is best-effort — Apple's pull API
// doesn't expose a cancellation token, so the underlying pull
// continues to completion even if ctx fires.
func (r *Runtime) PullImage(ctx context.Context, ref string, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	if err := ctx.Err(); err != nil {
		return runtime.ImageRef{}, err
	}
	if err := ensureLoaded(); err != nil {
		return runtime.ImageRef{}, err
	}
	emitBuildEvent(events, runtime.BuildEvent{
		Kind:    runtime.BuildEventLog,
		Message: "applecontainer: pulling " + ref,
	})

	cRef := C.CString(ref)
	defer C.free(unsafe.Pointer(cRef))
	raw := goStringAndFree(C.ac_pull_image_p(cRef))
	if raw == "" {
		return runtime.ImageRef{}, errors.New("applecontainer: bridge returned nil for PullImage")
	}
	env, err := decodeEnvelope[pullResultData](raw)
	if err != nil {
		return runtime.ImageRef{}, mapImageInspectErr(ref, err)
	}
	emitBuildEvent(events, runtime.BuildEvent{
		Kind:    runtime.BuildEventCompleted,
		Digest:  env.decoded.Digest,
		Message: "applecontainer: pulled " + env.decoded.Reference,
	})
	return runtime.ImageRef{
		ID:   env.decoded.Digest,
		Tags: []string{env.decoded.Reference},
	}, nil
}

// emitBuildEvent is a non-blocking send. Per the BuildEvent contract,
// events are best-effort: a slow consumer doesn't gate progress.
func emitBuildEvent(ch chan<- runtime.BuildEvent, ev runtime.BuildEvent) {
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	default:
	}
}
