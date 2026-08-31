package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/moby/moby/client"

	"github.com/crunchloop/devcontainer/runtime"
)

// Compose orchestrator primitives — Docker Engine API translations.
// All methods translate runtime-neutral *Spec / Filter types into
// moby client calls and back. Per the design (compose-native.md §4)
// the engine SDK is the only daemon-side concept we touch here; no
// docker compose shell-out, no compose-go.

// CreateNetwork creates a docker network. Idempotent on (name, label
// match) — if a network with the same name already exists and its
// labels are a superset of ours, we return its ID without
// recreating. Different-label collisions surface as a typed error.
func (r *Runtime) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) (string, error) {
	if spec.Name == "" {
		return "", errors.New("docker: NetworkSpec.Name is required")
	}

	// Pre-check by name. Networks aren't created with a "if missing"
	// flag, so the caller's idempotency expectation has to be
	// implemented here.
	existing, err := r.api.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("name", spec.Name),
	})
	if err != nil {
		return "", fmt.Errorf("NetworkList: %w", err)
	}
	for _, n := range existing.Items {
		if n.Name == spec.Name && labelsMatch(n.Labels, spec.Labels) {
			return n.ID, nil
		}
	}

	res, err := r.api.NetworkCreate(ctx, spec.Name, client.NetworkCreateOptions{
		Driver:  spec.Driver,
		Options: spec.Options,
		Labels:  spec.Labels,
	})
	if err != nil {
		return "", fmt.Errorf("NetworkCreate(%q): %w", spec.Name, err)
	}
	return res.ID, nil
}

// RemoveNetwork removes a network by ID. Missing-network errors are
// swallowed so callers can call this defensively at teardown.
func (r *Runtime) RemoveNetwork(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("docker: RemoveNetwork requires a network id")
	}
	if _, err := r.api.NetworkRemove(ctx, id, client.NetworkRemoveOptions{}); err != nil {
		if isNotFoundErr(err) {
			return nil
		}
		return fmt.Errorf("NetworkRemove(%q): %w", id, err)
	}
	return nil
}

// CreateVolume creates a named docker volume. Idempotent on (name,
// label match) — same shape as CreateNetwork.
func (r *Runtime) CreateVolume(ctx context.Context, spec runtime.VolumeSpec) (string, error) {
	if spec.Name == "" {
		return "", errors.New("docker: VolumeSpec.Name is required")
	}

	existing, err := r.api.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("name", spec.Name),
	})
	if err != nil {
		return "", fmt.Errorf("VolumeList: %w", err)
	}
	for _, v := range existing.Items {
		if v.Name == spec.Name && labelsMatch(v.Labels, spec.Labels) {
			return v.Name, nil
		}
	}

	res, err := r.api.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:       spec.Name,
		Driver:     spec.Driver,
		DriverOpts: spec.Options,
		Labels:     spec.Labels,
	})
	if err != nil {
		return "", fmt.Errorf("VolumeCreate(%q): %w", spec.Name, err)
	}
	return res.Volume.Name, nil
}

// RemoveVolume removes a named volume. Missing volumes are no-ops.
func (r *Runtime) RemoveVolume(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("docker: RemoveVolume requires a volume name")
	}
	if _, err := r.api.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil {
		if isNotFoundErr(err) {
			return nil
		}
		return fmt.Errorf("VolumeRemove(%q): %w", name, err)
	}
	return nil
}

// ListContainers returns containers matching every label in filter.
// Includes stopped containers — the orchestrator needs to find
// containers from prior Ups that may have exited.
func (r *Runtime) ListContainers(ctx context.Context, filter runtime.LabelFilter) ([]runtime.Container, error) {
	if len(filter.Match) == 0 {
		return nil, errors.New("docker: ListContainers requires a non-empty filter")
	}
	f := make(client.Filters)
	for k, v := range filter.Match {
		f.Add("label", k+"="+v)
	}
	res, err := r.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return nil, fmt.Errorf("ContainerList: %w", err)
	}
	out := make([]runtime.Container, 0, len(res.Items))
	for _, c := range res.Items {
		name := ""
		if len(c.Names) > 0 {
			name = trimLeadingSlash(c.Names[0])
		}
		out = append(out, runtime.Container{
			ID:     c.ID,
			Name:   name,
			Image:  c.Image,
			State:  mapContainerState(string(c.State)),
			Labels: copyLabels(c.Labels),
		})
	}
	return out, nil
}

// ListImages returns local images matching every label in filter.
// Used by Down --rmi local: built images carry a project label so
// teardown can prune by label.
func (r *Runtime) ListImages(ctx context.Context, filter runtime.LabelFilter) ([]runtime.ImageRef, error) {
	if len(filter.Match) == 0 {
		return nil, errors.New("docker: ListImages requires a non-empty filter")
	}
	f := make(client.Filters)
	for k, v := range filter.Match {
		f.Add("label", k+"="+v)
	}
	res, err := r.api.ImageList(ctx, client.ImageListOptions{
		All:     false, // intermediate layers aren't useful here
		Filters: f,
	})
	if err != nil {
		return nil, fmt.Errorf("ImageList: %w", err)
	}
	out := make([]runtime.ImageRef, 0, len(res.Items))
	for _, img := range res.Items {
		out = append(out, runtime.ImageRef{
			ID:   img.ID,
			Tags: append([]string(nil), img.RepoTags...),
		})
	}
	return out, nil
}

// RemoveImage removes a local image by ID or reference. Force=false
// matches compose's `down --rmi local` semantics — refuse to remove
// images that still have running containers attached.
func (r *Runtime) RemoveImage(ctx context.Context, ref string) error {
	if ref == "" {
		return errors.New("docker: RemoveImage requires an id or reference")
	}
	if _, err := r.api.ImageRemove(ctx, ref, client.ImageRemoveOptions{}); err != nil {
		if isImageNotFound(err) {
			return nil
		}
		return fmt.Errorf("ImageRemove(%q): %w", ref, err)
	}
	return nil
}

// labelsMatch returns true if `have` is a superset of `want`: every
// (k,v) in `want` is present and equal in `have`. Used by
// CreateNetwork / CreateVolume idempotency checks.
func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		// Explicit existence check: have[k] returns "" for missing
		// keys, which would falsely match want[k] == "" and let
		// CreateNetwork / CreateVolume reuse a label-less resource
		// when the caller actually requested an empty-valued label.
		hv, ok := have[k]
		if !ok || hv != v {
			return false
		}
	}
	return true
}

// mapContainerState mirrors the conversion already done in
// inspect.go for the docker API's lifecycle-state strings. Kept
// local to avoid widening that file's exports.
func mapContainerState(s string) runtime.State {
	switch s {
	case "created":
		return runtime.StateCreated
	case "running":
		return runtime.StateRunning
	case "paused":
		return runtime.StatePaused
	case "restarting":
		return runtime.StateRestarting
	case "removing":
		return runtime.StateRemoving
	case "exited":
		return runtime.StateExited
	case "dead":
		return runtime.StateDead
	}
	return runtime.State(s)
}

// Capabilities advertises the docker backend's compose behaviours.
// Both true: docker surfaces real exit codes on stopped containers and
// provides service-name DNS aliases on user-defined networks.
func (r *Runtime) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		Healthchecks:   true,
		ExitCodes:      true,
		ServiceNameDNS: true,
	}
}
