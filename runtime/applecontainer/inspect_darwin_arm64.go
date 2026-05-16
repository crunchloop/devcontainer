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
	"strings"
	"time"
	"unsafe"

	"github.com/crunchloop/devcontainer/runtime"
)

// ---- envelope -------------------------------------------------------

// envelope is the canonical wire shape the bridge returns for inspect
// and find exports. Documented in applecontainer-bridge/include/
// ac_bridge.h §header-comment style guide. Failure populates Err;
// success populates Data with the export-specific payload.
type envelope[T any] struct {
	OK   bool            `json:"ok"`
	Err  string          `json:"err"`
	Code string          `json:"code,omitempty"`
	Data json.RawMessage `json:"data"`
	// Decoded payload — populated by decodeEnvelope on success.
	decoded T
}

func decodeEnvelope[T any](raw string) (envelope[T], error) {
	var env envelope[T]
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return env, fmt.Errorf("applecontainer: malformed bridge response %q: %w", raw, err)
	}
	if !env.OK {
		return env, errors.New(env.Err)
	}
	// Allow null `data` (find-by-label miss).
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &env.decoded); err != nil {
			return env, fmt.Errorf("applecontainer: malformed bridge data %q: %w", string(env.Data), err)
		}
	}
	return env, nil
}

func goStringAndFree(c *C.char) string {
	if c == nil {
		return ""
	}
	s := C.GoString(c)
	C.ac_free_p(unsafe.Pointer(c))
	return s
}

// ---- Apple wire types ------------------------------------------------

// containerSnapshot mirrors the JSON shape Apple's
// ContainerSnapshot.Codable emits. Only the fields we use are listed;
// extras are ignored by encoding/json.
type containerSnapshot struct {
	Configuration containerConfiguration   `json:"configuration"`
	Status        string                   `json:"status"`
	StartedDate   *time.Time               `json:"startedDate,omitempty"`
	Networks      []containerNetworkAttach `json:"networks,omitempty"`
}

// containerNetworkAttach mirrors the per-attached-network shape on
// Apple's ContainerSnapshot.networks list. We only need the IPv4
// address; Apple emits it as a CIDR string ("192.168.66.2/24").
type containerNetworkAttach struct {
	IPv4Address string `json:"ipv4Address"`
}

type containerConfiguration struct {
	ID          string               `json:"id"`
	Image       imageDescription     `json:"image"`
	Mounts      []containerMount     `json:"mounts"`
	Labels      map[string]string    `json:"labels"`
	InitProcess containerInitProcess `json:"initProcess"`
}

type imageDescription struct {
	Reference string `json:"reference"`
}

type containerInitProcess struct {
	Environment      []string        `json:"environment"`
	WorkingDirectory string          `json:"workingDirectory"`
	User             json.RawMessage `json:"user"`
}

// containerMount mirrors the subset of Apple's Filesystem we need for
// MountInspect. Apple's Filesystem.type is a Codable enum-with-
// associated-values rendered as `{"virtiofs":{}}` / `{"tmpfs":{}}` /
// `{"block":{...}}` / `{"volume":{...}}`; mountTypeWire decodes
// either that object shape or a bare string so the rest of the file
// keeps treating Type as a kind tag.
type containerMount struct {
	Type        mountTypeWire `json:"type"`
	Source      string        `json:"source"`
	Destination string        `json:"destination"`
	Options     []string      `json:"options"`
}

// mountTypeWire decodes Apple's FSType. Always lands as the variant
// name in lowercase ("virtiofs", "tmpfs", "block", "volume", ...).
type mountTypeWire string

func (m *mountTypeWire) UnmarshalJSON(data []byte) error {
	// Compatibility path: an older bridge or a test fixture might emit
	// the kind as a plain string ("virtiofs"). Accept it first.
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		*m = mountTypeWire(asString)
		return nil
	}
	// Object form. The single key is the variant name; the value is
	// the associated-values payload, which we don't need yet.
	var asObject map[string]json.RawMessage
	if err := json.Unmarshal(data, &asObject); err != nil {
		return fmt.Errorf("mountTypeWire: %w (raw=%s)", err, string(data))
	}
	for k := range asObject {
		*m = mountTypeWire(k)
		return nil
	}
	return errors.New("mountTypeWire: empty FSType object")
}

type imageInspectPayload struct {
	Reference    string            `json:"reference"`
	Digest       string            `json:"digest"`
	Architecture string            `json:"architecture"`
	OS           string            `json:"os"`
	User         string            `json:"user"`
	Env          []string          `json:"env"`
	Labels       map[string]string `json:"labels"`
}

// ---- mapping helpers -------------------------------------------------

// mountKindToRuntime maps Apple's FSType variant name to the canonical
// runtime.MountType string. Apple uses `virtiofs` for bind-style host
// mounts; we surface that as "bind" since callers (engine, tests)
// reason about mounts in Docker-style terms.
func mountKindToRuntime(kind string) string {
	switch kind {
	case "virtiofs":
		return string(runtime.MountBind)
	case "tmpfs":
		return string(runtime.MountTmpfs)
	case "volume":
		return string(runtime.MountVolume)
	default:
		return kind
	}
}

func containsOption(opts []string, target string) bool {
	for _, o := range opts {
		if o == target {
			return true
		}
	}
	return false
}

func mapState(s string) runtime.State {
	switch s {
	case "running":
		return runtime.StateRunning
	case "stopping":
		return runtime.StateRemoving
	case "stopped":
		return runtime.StateExited
	default:
		return ""
	}
}

func snapshotToContainer(s containerSnapshot) *runtime.Container {
	return &runtime.Container{
		ID:    s.Configuration.ID,
		Name:  s.Configuration.ID, // Apple has no separate name field; id doubles
		Image: s.Configuration.Image.Reference,
		State: mapState(s.Status),
	}
}

func snapshotToDetails(s containerSnapshot) *runtime.ContainerDetails {
	mounts := make([]runtime.MountInspect, 0, len(s.Configuration.Mounts))
	for _, m := range s.Configuration.Mounts {
		mounts = append(mounts, runtime.MountInspect{
			Type:     mountKindToRuntime(string(m.Type)),
			Source:   m.Source,
			Target:   m.Destination,
			ReadOnly: containsOption(m.Options, "ro"),
		})
	}
	var startedAt time.Time
	if s.StartedDate != nil {
		startedAt = *s.StartedDate
	}
	// Compose orchestrator looks up the container's primary IPv4
	// address through the well-known label key
	// `dev.containers.network-ip` (see compose.Orchestrator's
	// /etc/hosts patch). Apple's ContainerSnapshot exposes the
	// address under networks[].ipv4Address in CIDR form
	// ("192.168.66.2/24"); we strip the prefix and stash it in the
	// labels map so the orchestrator doesn't need a typed network
	// field on runtime.ContainerDetails. Labels we synthesize
	// never override user-set labels with the same key.
	labels := s.Configuration.Labels
	if ip := primaryIPv4(s.Networks); ip != "" {
		if labels == nil {
			labels = map[string]string{}
		}
		if _, exists := labels["dev.containers.network-ip"]; !exists {
			labels["dev.containers.network-ip"] = ip
		}
	}

	return &runtime.ContainerDetails{
		Container: *snapshotToContainer(s),
		StartedAt: startedAt,
		User:      decodeUserString(s.Configuration.InitProcess.User),
		Env:       s.Configuration.InitProcess.Environment,
		Mounts:    mounts,
		Labels:    labels,
		// Created / FinishedAt / ExitCode are not in Apple's
		// ContainerSnapshot. Left as zero values; later PRs can
		// surface them via an additional XPC call if exposed.
	}
}

// primaryIPv4 returns the first non-empty network attachment's IP
// stripped of its CIDR prefix. Apple's ContainerSnapshot
// typically reports a single attachment per container.
func primaryIPv4(nets []containerNetworkAttach) string {
	for _, n := range nets {
		if n.IPv4Address == "" {
			continue
		}
		if i := strings.Index(n.IPv4Address, "/"); i > 0 {
			return n.IPv4Address[:i]
		}
		return n.IPv4Address
	}
	return ""
}

// decodeUserString turns Apple's ProcessConfiguration.User Codable
// representation into a single string. The Codable shape is either
// {"raw":{"userString":"..."}} or {"id":{"uid":N,"gid":N}}; either way
// the description renders to "user[:group]" or "uid:gid".
func decodeUserString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var pair struct {
		Raw *struct {
			UserString string `json:"userString"`
		} `json:"raw"`
		ID *struct {
			UID uint32 `json:"uid"`
			GID uint32 `json:"gid"`
		} `json:"id"`
	}
	if err := json.Unmarshal(raw, &pair); err != nil {
		return ""
	}
	if pair.Raw != nil {
		return pair.Raw.UserString
	}
	if pair.ID != nil {
		return fmt.Sprintf("%d:%d", pair.ID.UID, pair.ID.GID)
	}
	return ""
}

// ---- Runtime methods -------------------------------------------------

// InspectContainer fetches a snapshot from the apiserver and projects
// it into our runtime.ContainerDetails. Returns
// *runtime.ContainerNotFoundError when the daemon reports "not found".
func (r *Runtime) InspectContainer(ctx context.Context, id string) (*runtime.ContainerDetails, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLoaded(); err != nil {
		return nil, err
	}
	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))
	raw := goStringAndFree(C.ac_inspect_container_p(cID))
	if raw == "" {
		return nil, errors.New("applecontainer: bridge returned nil for InspectContainer")
	}
	env, err := decodeEnvelope[containerSnapshot](raw)
	if err != nil {
		return nil, mapInspectErr(id, err)
	}
	return snapshotToDetails(env.decoded), nil
}

// InspectImage fetches OCI config metadata for a locally-cached image.
// Critical path: caller reads env.Labels["devcontainer.metadata"] to
// short-circuit feature installs against pre-baked images.
func (r *Runtime) InspectImage(ctx context.Context, ref string) (*runtime.ImageDetails, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLoaded(); err != nil {
		return nil, err
	}
	cRef := C.CString(ref)
	defer C.free(unsafe.Pointer(cRef))
	raw := goStringAndFree(C.ac_inspect_image_p(cRef))
	if raw == "" {
		return nil, errors.New("applecontainer: bridge returned nil for InspectImage")
	}
	env, err := decodeEnvelope[imageInspectPayload](raw)
	if err != nil {
		return nil, mapImageInspectErr(ref, err)
	}
	p := env.decoded
	return &runtime.ImageDetails{
		ID:     p.Digest,
		Tags:   []string{p.Reference},
		Labels: p.Labels,
		Env:    p.Env,
		User:   p.User,
	}, nil
}

// FindContainerByLabel returns the most recently started container
// whose configuration.labels[key] == value, or nil if no match.
func (r *Runtime) FindContainerByLabel(ctx context.Context, key, value string) (*runtime.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLoaded(); err != nil {
		return nil, err
	}
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	cVal := C.CString(value)
	defer C.free(unsafe.Pointer(cVal))
	raw := goStringAndFree(C.ac_find_container_by_label_p(cKey, cVal))
	if raw == "" {
		return nil, errors.New("applecontainer: bridge returned nil for FindContainerByLabel")
	}
	env, err := decodeEnvelope[containerSnapshot](raw)
	if err != nil {
		return nil, err
	}
	// `data: null` means "no match" — env.Data starts as a nil
	// RawMessage in that case and decodedEnvelope skipped decoding, so
	// env.decoded is the zero ContainerSnapshot. Detect by checking
	// the parsed configuration id.
	if env.decoded.Configuration.ID == "" {
		return nil, nil
	}
	return snapshotToContainer(env.decoded), nil
}

// ---- error mapping ---------------------------------------------------

func mapInspectErr(id string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// Apple's not-found errors look like:
	//   `notFound: "container with ID <id> not found"`
	// or contain "not found" — match loosely so future wording shifts
	// don't drop us out of the typed-error contract.
	if containsAny(msg, "notFound", "not found") {
		return &runtime.ContainerNotFoundError{ID: id, Err: err}
	}
	return err
}

func mapImageInspectErr(ref string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if containsAny(msg, "notFound", "not found") {
		return &runtime.ImageNotFoundError{Ref: ref, Err: err}
	}
	return err
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

// indexOf is strings.Contains/strings.Index minus the import — keeps
// this file's deps narrow (and avoids re-importing strings for this
// one use). Inline implementation, O(n*m), fine for our short error
// strings.
func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(s) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
