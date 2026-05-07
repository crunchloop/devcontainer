package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"

	"github.com/crunchloop/devcontainer/runtime"
)

func (r *Runtime) PullImage(ctx context.Context, ref string, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	resp, err := r.api.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		if isImageNotFound(err) {
			return runtime.ImageRef{}, &runtime.ImageNotFoundError{Ref: ref, Err: err}
		}
		return runtime.ImageRef{}, fmt.Errorf("ImagePull: %w", err)
	}
	defer resp.Close()

	// Stream JSON messages, mapping each to a BuildEvent. The iter is
	// driven by reading the underlying stream; finishing the iteration
	// is what proves the pull completed.
	for msg, mErr := range resp.JSONMessages(ctx) {
		if mErr != nil {
			return runtime.ImageRef{}, fmt.Errorf("pull stream: %w", mErr)
		}
		if msg.Error != nil {
			return runtime.ImageRef{}, fmt.Errorf("pull failed: %s", msg.Error.Message)
		}
		emitBuildEvent(events, runtime.BuildEvent{
			Kind:    runtime.BuildEventPullProgress,
			Message: msg.Status,
			LayerID: msg.ID,
		})
	}

	// After the stream completes the image is present locally; resolve to
	// its digest+tags via inspect.
	inspectRes, err := r.api.ImageInspect(ctx, ref)
	if err != nil {
		return runtime.ImageRef{}, fmt.Errorf("ImageInspect %s: %w", ref, err)
	}
	emitBuildEvent(events, runtime.BuildEvent{
		Kind:   runtime.BuildEventCompleted,
		Digest: inspectRes.ID,
	})
	return runtime.ImageRef{
		ID:   inspectRes.ID,
		Tags: inspectRes.RepoTags,
	}, nil
}

// emitBuildEvent sends an event to events if the channel is non-nil and
// has buffer space. We deliberately drop on full to keep the runtime
// non-blocking when the consumer isn't keeping up.
func emitBuildEvent(events chan<- runtime.BuildEvent, ev runtime.BuildEvent) {
	if events == nil {
		return
	}
	select {
	case events <- ev:
	default:
	}
}
