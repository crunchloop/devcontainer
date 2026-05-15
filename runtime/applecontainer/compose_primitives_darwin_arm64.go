//go:build darwin && arm64

package applecontainer

import (
	"context"

	"github.com/crunchloop/devcontainer/runtime"
)

// This file holds stubs for the compose orchestrator primitives
// declared on runtime.Runtime. Real implementations are PR15 work
// (post the §11 probe validation against the merged runtime). For
// now they return ErrNotImplemented; Capabilities() advertises the
// known-false set per design/compose-native.md §11.5 so the Plan
// validator refuses compose features apple cannot currently honor.
//
// When PR15 lands, each capability flips true independently as the
// upstream apple/container issue closes and our implementation
// catches up.

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

// Capabilities reports the apple-container backend's compose
// feature support as of apple/container 0.12.x. Every flag is
// false until §4 primitives + the probe workarounds are
// implemented (PR15). See design/compose-native.md §11.5 for
// per-flag provenance.
func (r *Runtime) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		Healthchecks:     false,
		ExitCodes:        false,
		NamespaceSharing: false,
		RestartPolicies:  false,
		SharedVolumes:    false,
	}
}
