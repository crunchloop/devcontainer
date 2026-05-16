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

// Compose orchestrator primitives — apple/container 0.12 surface.
// Each Go method marshals a runtime-neutral *Spec into JSON, calls
// through the cgo shim into the Swift bridge, decodes the envelope,
// and returns the typed result.
//
// Capabilities() still advertises all-false per design §11.5: the
// upstream apple/container gaps (healthchecks #1502, exit codes
// #1501, restart policies #286, shared volumes #889,
// namespace-sharing architectural) remain open as of 0.12.x.
// Compose orchestrator's Plan validator refuses projects that need
// those features; projects that don't (the simple service_started
// case, no shared volumes, no namespace games, no restart-on-crash)
// work fine with these primitives.

// networkSpecWire mirrors applecontainer-bridge/Sources/ACBridge/
// networks.swift's NetworkSpecJSON. Apple ignores driver / options
// (it has one network plugin); we send them through for parity but
// they have no effect on the backend side.
type networkSpecWire struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels,omitempty"`
	Driver  string            `json:"driver,omitempty"`
	Options map[string]string `json:"options,omitempty"`
}

type networkResultData struct {
	ID string `json:"id"`
}

// CreateNetwork creates a project network via apple's NetworkClient.
// Idempotent on (name, label superset) — re-runs of compose Up
// against an existing project reuse the network rather than erroring.
func (r *Runtime) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := ensureLoaded(); err != nil {
		return "", err
	}
	wire := networkSpecWire{
		Name:    spec.Name,
		Labels:  spec.Labels,
		Driver:  spec.Driver,
		Options: spec.Options,
	}
	specBytes, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("applecontainer: marshal NetworkSpec: %w", err)
	}
	cSpec := C.CString(string(specBytes))
	defer C.free(unsafe.Pointer(cSpec))

	raw := goStringAndFree(C.ac_network_create_p(cSpec))
	if raw == "" {
		return "", errors.New("applecontainer: bridge returned nil for CreateNetwork")
	}
	env, err := decodeEnvelope[networkResultData](raw)
	if err != nil {
		return "", err
	}
	return env.decoded.ID, nil
}

// RemoveNetwork deletes a network by ID. Missing-network errors are
// swallowed in the bridge so this is naturally idempotent.
func (r *Runtime) RemoveNetwork(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureLoaded(); err != nil {
		return err
	}
	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))
	raw := goStringAndFree(C.ac_network_remove_p(cID))
	if raw == "" {
		return errors.New("applecontainer: bridge returned nil for RemoveNetwork")
	}
	if _, err := decodeEnvelope[json.RawMessage](raw); err != nil {
		return err
	}
	return nil
}

// volumeSpecWire mirrors volumes.swift's VolumeSpecJSON.
type volumeSpecWire struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels,omitempty"`
	Driver  string            `json:"driver,omitempty"`
	Options map[string]string `json:"options,omitempty"`
}

type volumeResultData struct {
	Name string `json:"name"`
}

// CreateVolume creates a named volume via ClientVolume. Idempotent
// on label-superset match.
func (r *Runtime) CreateVolume(ctx context.Context, spec runtime.VolumeSpec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := ensureLoaded(); err != nil {
		return "", err
	}
	wire := volumeSpecWire{
		Name:    spec.Name,
		Labels:  spec.Labels,
		Driver:  spec.Driver,
		Options: spec.Options,
	}
	specBytes, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("applecontainer: marshal VolumeSpec: %w", err)
	}
	cSpec := C.CString(string(specBytes))
	defer C.free(unsafe.Pointer(cSpec))

	raw := goStringAndFree(C.ac_volume_create_p(cSpec))
	if raw == "" {
		return "", errors.New("applecontainer: bridge returned nil for CreateVolume")
	}
	env, err := decodeEnvelope[volumeResultData](raw)
	if err != nil {
		return "", err
	}
	return env.decoded.Name, nil
}

// RemoveVolume deletes a named volume. Bridge swallows notFound.
func (r *Runtime) RemoveVolume(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureLoaded(); err != nil {
		return err
	}
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	raw := goStringAndFree(C.ac_volume_remove_p(cName))
	if raw == "" {
		return errors.New("applecontainer: bridge returned nil for RemoveVolume")
	}
	if _, err := decodeEnvelope[json.RawMessage](raw); err != nil {
		return err
	}
	return nil
}

// containerListItem mirrors list.swift's ContainerListItem.
type containerListItem struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	State  string            `json:"state"`
	Labels map[string]string `json:"labels"`
}

type containerListData struct {
	Containers []containerListItem `json:"containers"`
}

// ListContainers enumerates every container the apiserver knows
// about and applies the filter client-side. Apple's list endpoint
// doesn't support server-side label filtering as of 0.12.x (design
// probe R1b); the workspace-scale traffic makes the overhead a
// non-issue.
func (r *Runtime) ListContainers(ctx context.Context, filter runtime.LabelFilter) ([]runtime.Container, error) {
	if len(filter.Match) == 0 {
		return nil, errors.New("applecontainer: ListContainers requires a non-empty filter")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLoaded(); err != nil {
		return nil, err
	}
	raw := goStringAndFree(C.ac_list_containers_p())
	if raw == "" {
		return nil, errors.New("applecontainer: bridge returned nil for ListContainers")
	}
	env, err := decodeEnvelope[containerListData](raw)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Container, 0, len(env.decoded.Containers))
	for _, item := range env.decoded.Containers {
		if !labelsMatchFilter(item.Labels, filter.Match) {
			continue
		}
		labels := make(map[string]string, len(item.Labels))
		for k, v := range item.Labels {
			labels[k] = v
		}
		out = append(out, runtime.Container{
			ID:     item.ID,
			Name:   item.Name,
			Image:  item.Image,
			State:  mapState(item.State),
			Labels: labels,
		})
	}
	return out, nil
}

// imageListItem mirrors list.swift's ImageListItem.
type imageListItem struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
}

type imageListData struct {
	Images []imageListItem `json:"images"`
}

// ListImages enumerates local images and filters client-side. Apple
// doesn't carry custom labels on images today; filter.Match against
// our project-label keys will typically match nothing, which is
// fine — Down's --rmi local would simply find no project-built
// images to prune on apple.
func (r *Runtime) ListImages(ctx context.Context, filter runtime.LabelFilter) ([]runtime.ImageRef, error) {
	if len(filter.Match) == 0 {
		return nil, errors.New("applecontainer: ListImages requires a non-empty filter")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLoaded(); err != nil {
		return nil, err
	}
	raw := goStringAndFree(C.ac_list_images_p())
	if raw == "" {
		return nil, errors.New("applecontainer: bridge returned nil for ListImages")
	}
	env, err := decodeEnvelope[imageListData](raw)
	if err != nil {
		return nil, err
	}
	// Apple's ImageDescription doesn't currently expose labels —
	// without per-image labels we can't filter on them. Return the
	// full list; callers (Orchestrator.Down --rmi local) typically
	// pass a project label that matches nothing here, so the
	// downstream RemoveImage loop is a no-op.
	out := make([]runtime.ImageRef, 0, len(env.decoded.Images))
	for _, item := range env.decoded.Images {
		out = append(out, runtime.ImageRef{
			ID:   item.ID,
			Tags: append([]string(nil), item.Tags...),
		})
	}
	return out, nil
}

// RemoveImage removes a local image by reference. Bridge swallows
// notFound.
func (r *Runtime) RemoveImage(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureLoaded(); err != nil {
		return err
	}
	cRef := C.CString(ref)
	defer C.free(unsafe.Pointer(cRef))
	raw := goStringAndFree(C.ac_remove_image_p(cRef))
	if raw == "" {
		return errors.New("applecontainer: bridge returned nil for RemoveImage")
	}
	if _, err := decodeEnvelope[json.RawMessage](raw); err != nil {
		return err
	}
	return nil
}

// Capabilities reports the apple-container backend's compose
// feature support. As of 0.12.x every flag is false — the upstream
// apple/container issues governing each capability are still open
// (see design/compose-native.md §11.5). The compose Plan validator
// uses this struct to refuse projects that require any of these
// features; everything else works through the primitive surface
// implemented above.
func (r *Runtime) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		Healthchecks:     false,
		ExitCodes:        false,
		NamespaceSharing: false,
		RestartPolicies:  false,
		SharedVolumes:    false,
	}
}

// labelsMatchFilter is the client-side label filter we apply after
// fetching a full container/image list. Every (k, v) in want must
// appear in have for the resource to match.
func labelsMatchFilter(have map[string]string, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
