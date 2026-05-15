package docker

import (
	"context"

	"github.com/crunchloop/devcontainer/runtime"
)

// This file holds stubs for the compose orchestrator primitives
// declared on runtime.Runtime in PR13. Real implementations land in
// the next commit; for now each method returns ErrNotImplemented so
// the interface compiles and bisect stays green.
//
// Capabilities() returns the docker baseline (all true) so once the
// real implementations land, compose.Plan.Validate immediately
// accepts every feature on this backend.

// CreateNetwork is a stub. See runtime.Runtime.CreateNetwork.
func (r *Runtime) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) (string, error) {
	return "", runtime.ErrNotImplemented
}

// RemoveNetwork is a stub. See runtime.Runtime.RemoveNetwork.
func (r *Runtime) RemoveNetwork(ctx context.Context, id string) error {
	return runtime.ErrNotImplemented
}

// CreateVolume is a stub. See runtime.Runtime.CreateVolume.
func (r *Runtime) CreateVolume(ctx context.Context, spec runtime.VolumeSpec) (string, error) {
	return "", runtime.ErrNotImplemented
}

// RemoveVolume is a stub. See runtime.Runtime.RemoveVolume.
func (r *Runtime) RemoveVolume(ctx context.Context, name string) error {
	return runtime.ErrNotImplemented
}

// ListContainers is a stub. See runtime.Runtime.ListContainers.
func (r *Runtime) ListContainers(ctx context.Context, filter runtime.LabelFilter) ([]runtime.Container, error) {
	return nil, runtime.ErrNotImplemented
}

// ListImages is a stub. See runtime.Runtime.ListImages.
func (r *Runtime) ListImages(ctx context.Context, filter runtime.LabelFilter) ([]runtime.ImageRef, error) {
	return nil, runtime.ErrNotImplemented
}

// RemoveImage is a stub. See runtime.Runtime.RemoveImage.
func (r *Runtime) RemoveImage(ctx context.Context, ref string) error {
	return runtime.ErrNotImplemented
}

// Capabilities advertises the docker backend's compose feature set.
// Every flag is true: docker exposes healthchecks, exit codes,
// namespace sharing, restart policies, and shared volumes — the full
// baseline the orchestrator expects.
func (r *Runtime) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		Healthchecks:     true,
		ExitCodes:        true,
		NamespaceSharing: true,
		RestartPolicies:  true,
		SharedVolumes:    true,
	}
}
