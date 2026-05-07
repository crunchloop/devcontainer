package docker

import (
	"context"

	"github.com/crunchloop/devcontainer/runtime"
)

// BuildImage is not implemented in this milestone — it lands in M3 with
// feature dockerfile generation. Image-source devcontainers go through
// PullImage; build-source configs are rejected at the Engine layer with
// runtime.ErrNotImplemented before reaching this method.
func (r *Runtime) BuildImage(ctx context.Context, spec runtime.BuildSpec, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	return runtime.ImageRef{}, runtime.ErrNotImplemented
}
