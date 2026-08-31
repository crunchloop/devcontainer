package runtime

// This file declares the runtime-neutral types consumed by the new
// network / volume / list / capability primitives that the
// compose orchestrator (compose/) drives. The methods themselves are
// declared on the Runtime interface in runtime.go; this file holds
// the input/output shapes so backends can translate without leaking
// Docker-API types into the orchestrator.
//
// Naming follows the existing Spec/Details pattern: backends accept
// *Spec inputs and return their backend ID or a typed error.

// NetworkSpec describes a user-defined network to create. The
// compose orchestrator creates exactly one project network per Up
// (<project>_default) plus any volumes; multi-network compose
// projects are §2.2 refused (see design/compose-native.md).
type NetworkSpec struct {
	// Name is the user-facing network name (e.g. dc-<id>_default).
	// Backends namespace internally if their model requires it; the
	// returned ID is what callers store for later RemoveNetwork.
	Name string

	// Labels are stamped on the network for identification at
	// teardown. The orchestrator uses com.docker.compose.project +
	// dev.containers.engine labels exclusively; backends pass them
	// through verbatim.
	Labels map[string]string

	// Driver selects the backend's network driver. Empty string means
	// "backend default" (bridge on docker). Non-default drivers are
	// out of scope for v1.
	Driver string

	// Options is the driver-options string map (compose's
	// driver_opts:). Pass-through; backends may reject unknown keys
	// with a typed error.
	Options map[string]string
}

// VolumeSpec describes a named volume to create. The orchestrator
// creates one per top-level compose `volumes:` entry actually
// referenced by a service. Anonymous volumes are handled at
// RunSpec.Mounts time, not here.
type VolumeSpec struct {
	// Name is the user-facing volume name (e.g. dc-<id>_<volname>).
	// Backends translate to whatever their native naming requires.
	Name string

	// Labels for teardown lookup. Same convention as NetworkSpec.
	Labels map[string]string

	// Driver selects the backend's volume driver. Empty = backend
	// default (local on docker).
	Driver string

	// Options is the driver-options string map. Pass-through.
	Options map[string]string
}

// LabelFilter selects containers, images, or volumes by an AND of
// label key/value pairs. The Runtime contract requires non-empty;
// the orchestrator never wants to enumerate everything.
type LabelFilter struct {
	// Match is the AND set: every key must be present on the
	// resource AND its value must equal the requested value.
	// Implementations that lack server-side filtering translate this
	// client-side after enumeration.
	Match map[string]string
}
