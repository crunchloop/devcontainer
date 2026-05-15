package runtime

// This file declares the runtime-neutral types consumed by the new
// network / volume / list / capability primitives that the
// compose orchestrator (compose/) drives. The methods themselves are
// declared on the Runtime interface in runtime.go; this file holds
// the input/output shapes so backends can translate without leaking
// Docker-API or Apple-bridge types into the orchestrator.
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
	// "backend default" (bridge on docker; vmnet-based default on
	// apple). Non-default drivers are out of scope for v1.
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
	// default (local on docker; the file-backed driver on apple).
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
	// Implementations that lack server-side filtering (apple, per
	// design probe R1b) translate this client-side after enumeration.
	Match map[string]string
}

// Capabilities advertises optional features a backend implements.
// The compose orchestrator's plan validator (compose.Plan.Validate)
// keys feature gates off this struct so per-backend conditionals
// stay out of the validator. Backends self-describe; defaults are
// the docker baseline.
//
// Each field documents the upstream issue (or status note) governing
// it so future contributors can tell at a glance which capabilities
// might flip true on the apple backend in the future. See
// design/compose-native.md §11.5 for the full provenance.
type Capabilities struct {
	// Healthchecks: backend honors HEALTHCHECK directives on
	// RunSpec/BuildSpec, and InspectContainer surfaces
	// State.Health.Status. Required for compose's
	// depends_on.<svc>.condition: service_healthy gating.
	//
	// Apple 0.12.x: false (apple/container #1502).
	Healthchecks bool

	// ExitCodes: InspectContainer returns the container's exit code
	// after Stop (ContainerDetails.ExitCode is meaningful for
	// state=exited). Required for compose's depends_on condition:
	// service_completed_successfully.
	//
	// Apple 0.12.x: false (apple/container #1501).
	ExitCodes bool

	// NamespaceSharing: backend supports network_mode / pid / ipc
	// set to service:<other> (Linux namespace sharing within one
	// kernel).
	//
	// Apple: architectural false — one VM per container means
	// separate kernels; namespace sharing is not implementable.
	NamespaceSharing bool

	// RestartPolicies: backend enforces compose's `restart:` field
	// via RunSpec or backend-equivalent. When false, the
	// orchestrator emits a single WarnRestartPolicyIgnoredOnBackend
	// event per Plan rather than refusing the project.
	//
	// Apple 0.12.x: false (apple/container #286).
	RestartPolicies bool

	// SharedVolumes: a single named volume can be concurrently
	// mounted into 2+ running containers. Apple's
	// ext4-on-disk-image volumes refuse multi-attach with
	// VZErrorDomain Code=2; Plan.Validate refuses such projects on
	// backends where this is false.
	//
	// Apple 0.12.x: false (apple/container #889).
	SharedVolumes bool
}
