package runtime

import (
	"context"
	"io"
	"time"
)

// ComposeRuntime is the optional sub-interface a Runtime implements
// when it can drive a Docker Compose project. Engine.Up type-asserts
// this when handling *config.ComposeSource and returns
// runtime.ErrNotImplemented if the active runtime doesn't satisfy it.
//
// All compose orchestration goes through these three calls plus a
// version probe (handled internally by the implementation). Container
// interaction (Exec, Inspect, Logs, lifecycle markers) stays on the
// regular Runtime methods, against the container id resolved by
// ComposeContainerID.
type ComposeRuntime interface {
	// ComposeUp brings the project up in the background (-d).
	// Compose decides what to (re)build via its own logic; we feed it
	// override files that pin the primary service's image to a tag we
	// already built, so it does not rebuild that one.
	ComposeUp(ctx context.Context, spec ComposeUpSpec, events chan<- BuildEvent) error

	// ComposeDown stops and (optionally) removes the project's
	// containers, networks, and volumes.
	ComposeDown(ctx context.Context, spec ComposeDownSpec) error

	// ComposeContainerID returns the container id for a service in a
	// running project. Used by Engine.Up to pick out the primary
	// service's container after `compose up -d` settles. Returns
	// empty string if the service isn't running.
	ComposeContainerID(ctx context.Context, spec ComposePsSpec, service string) (string, error)
}

// ComposeUpSpec configures ComposeUp.
type ComposeUpSpec struct {
	// Files lists compose files in declaration order — user files
	// first, then our generated overrides (build, run). Each becomes
	// a `-f <path>` flag.
	Files []string

	// ProjectName is passed via -p / --project-name. Engine derives
	// it from the workspace id (`dc-<devcontainerId>` per PRD §12.5).
	ProjectName string

	// Services optionally restricts which services to start. Empty =
	// all services in the project (compose default).
	Services []string

	// WorkingDir is the directory `docker compose` runs in. Used as
	// the base for compose's relative-path resolution. Engine sets
	// this to the workspace folder.
	WorkingDir string

	// NoRecreate, when true, appends `--no-recreate` to the compose
	// invocation. Tells compose to keep an existing container even if
	// it thinks the config drifted — used on the resume path so we
	// don't destroy the container's writable layer (and anything in
	// $HOME inside it) on a spurious drift detection. Matches the
	// upstream devcontainers/cli gate (`container || expectExistingContainer`).
	NoRecreate bool
}

// ComposeDownSpec configures ComposeDown.
type ComposeDownSpec struct {
	Files       []string
	ProjectName string
	WorkingDir  string

	// RemoveImages, when true, passes --rmi local (removes images
	// the project built locally; spares pulled images).
	RemoveImages bool

	// RemoveVolumes, when true, passes --volumes (removes named
	// volumes declared by the project plus anonymous volumes).
	RemoveVolumes bool
}

// ComposePsSpec configures ComposeContainerID.
type ComposePsSpec struct {
	Files       []string
	ProjectName string
	WorkingDir  string
}

// CheckpointRuntime is the optional sub-interface a Runtime implements
// when it can checkpoint a running container to a portable archive
// (process + memory state via CRIU, plus the writable rootfs layer) and
// later restore it — possibly on another host — into a fresh container.
//
// Implemented by runtime/podman (podman container checkpoint --export /
// restore --import). NOT implemented by runtime/docker: docker's restore
// is broken on current containerd-integrated engines (see
// design/checkpoint-restore.md). Engine.Checkpoint / Engine.Restore
// type-assert this and return ErrCheckpointUnsupported when the active
// runtime doesn't satisfy it (or advertises Capabilities().Checkpoint
// == false).
type CheckpointRuntime interface {
	// Checkpoint writes a self-contained checkpoint archive for a
	// running container to spec.ArchivePath. The archive carries the
	// CRIU image, the writable rootfs diff, and the config needed to
	// restore. With spec.StopAfter the container is stopped/removed
	// after the archive is written (the spot-eviction path); otherwise
	// it keeps running ("backup" checkpoint).
	Checkpoint(ctx context.Context, id string, spec CheckpointSpec) (CheckpointRef, error)

	// Restore re-creates and resumes a container from a checkpoint
	// archive, reconstructing its mounts and re-attaching networking.
	// Restores into a NEW container (migration), so the source may be
	// gone. Returns the new Container handle.
	Restore(ctx context.Context, spec RestoreSpec) (*Container, error)
}

// CheckpointSpec configures CheckpointRuntime.Checkpoint.
type CheckpointSpec struct {
	// ArchivePath is the file the export archive is written to. The
	// archive is self-contained and node-independent, so cross-node
	// restore is just moving this file — point it at durable,
	// transferable storage (the workspace volume, object storage).
	ArchivePath string

	// StopAfter stops/removes the container after a successful export
	// (the spot-eviction path: the node is going away). False keeps the
	// container running.
	StopAfter bool

	// TCPEstablished requests checkpoint of established TCP connections.
	// Required for any container holding a live connection at checkpoint
	// time — without it the checkpoint fails. Reconnecting clients
	// recover regardless; a persistent connection across a peer-IP
	// change on restore is the residual edge.
	TCPEstablished bool
}

// RestoreSpec configures CheckpointRuntime.Restore. The archive is
// self-describing (image, config, mounts, rootfs), so no RunSpec is
// needed — unlike a cold create.
type RestoreSpec struct {
	// ArchivePath is the archive a prior Checkpoint wrote.
	ArchivePath string

	// Name optionally names the restored container. Empty lets the
	// backend choose (or reuse the archived name).
	Name string

	// TCPEstablished must match the checkpoint when the archive captured
	// established connections.
	TCPEstablished bool

	// IgnoreVolumes asks the backend to skip restoring volume content
	// from the archive, reusing whatever volume already exists. For
	// Podman this maps to restore's ignore-volumes; cross-node restore
	// leaves it false (content must come from the archive), same-node
	// restore-in-place sets it true to avoid a "volume already exists"
	// collision.
	IgnoreVolumes bool
}

// CheckpointRef describes a written checkpoint archive.
type CheckpointRef struct {
	// ArchivePath echoes where the archive was written.
	ArchivePath string

	// Size is the archive size in bytes — feeds the caller's
	// eviction-window / transfer budgeting. Best-effort; 0 if unknown.
	Size int64
}

// Runtime is the container backend. Implementations must be safe for
// concurrent use; the engine may issue concurrent Inspect / Exec calls
// against the same container.
type Runtime interface {
	// BuildImage builds an image from a build spec and returns its
	// reference. Build progress is streamed on events (best-effort; events
	// may be dropped if the channel is full).
	BuildImage(ctx context.Context, spec BuildSpec, events chan<- BuildEvent) (ImageRef, error)

	// PullImage fetches an image from a registry, returning its reference.
	// Pull progress is streamed on events.
	PullImage(ctx context.Context, ref string, events chan<- BuildEvent) (ImageRef, error)

	// RunContainer creates a container from a run spec. The container is
	// not started — call StartContainer when ready. Splitting create from
	// start lets callers attach hooks (e.g. mark files) between phases.
	RunContainer(ctx context.Context, spec RunSpec) (*Container, error)

	// StartContainer starts a previously created container.
	StartContainer(ctx context.Context, id string) error

	// StopContainer stops a running container, waiting up to opts.Timeout
	// for graceful shutdown before SIGKILL.
	StopContainer(ctx context.Context, id string, opts StopOptions) error

	// RemoveContainer removes a container. The container must be stopped
	// unless opts.Force is set.
	RemoveContainer(ctx context.Context, id string, opts RemoveOptions) error

	// ExecContainer runs a command inside a running container. If
	// opts.Stdout / Stderr are nil, output is captured into ExecResult.
	ExecContainer(ctx context.Context, id string, opts ExecOptions) (ExecResult, error)

	// InspectContainer returns full details for a container, including
	// the live env (used for ${containerEnv:*} substitution).
	InspectContainer(ctx context.Context, id string) (*ContainerDetails, error)

	// InspectImage returns the image's labels, env, and other config.
	// Used by the engine to read the devcontainer.metadata label off
	// pre-baked images and skip already-installed features.
	InspectImage(ctx context.Context, ref string) (*ImageDetails, error)

	// ContainerLogs streams the container's stdout+stderr to w. If follow
	// is true, the call blocks until ctx is cancelled or the container exits.
	ContainerLogs(ctx context.Context, id string, w io.Writer, follow bool) error

	// FindContainerByLabel returns the most recently created container
	// matching the given label. Returns nil, nil if no match.
	FindContainerByLabel(ctx context.Context, key, value string) (*Container, error)

	// ---- compose orchestration primitives -----------------------------
	//
	// Methods below are consumed by the runtime-agnostic compose
	// orchestrator under compose/ (see design/compose-native.md §4).
	// Types live in runtime/compose_primitives.go. A backend that
	// returns ErrNotImplemented from any of these effectively opts
	// out of compose source — Plan.Validate(Capabilities()) catches
	// such projects at validation time and refuses with a typed
	// error before any side effect.

	// CreateNetwork creates a network with the given name and
	// labels. Returns the backend's network ID for later
	// RemoveNetwork. Idempotent on (name, labels) match: if a
	// network with the same name and matching label set already
	// exists, return its ID without error (same shape as compose's
	// own up behavior).
	CreateNetwork(ctx context.Context, spec NetworkSpec) (string, error)

	// RemoveNetwork removes a network by its backend ID. No-op if
	// the network is already gone.
	RemoveNetwork(ctx context.Context, id string) error

	// CreateVolume creates a named volume. Idempotent on (name,
	// labels). Returns the backend's volume identifier — usually
	// the name itself, but backends may translate.
	CreateVolume(ctx context.Context, spec VolumeSpec) (string, error)

	// RemoveVolume removes a named volume. No-op if missing.
	RemoveVolume(ctx context.Context, name string) error

	// ListContainers returns containers matching every label in the
	// filter. Empty filter is rejected — we never want to enumerate
	// all containers. Implementations without server-side filtering
	// (e.g. applecontainer per design probe R1b) filter client-side
	// after a full enumeration.
	ListContainers(ctx context.Context, filter LabelFilter) ([]Container, error)

	// ListImages returns local images matching the filter. Used by
	// Down --rmi local: built images are stamped with project labels
	// so teardown can prune by label. Same empty-filter rule as
	// ListContainers.
	ListImages(ctx context.Context, filter LabelFilter) ([]ImageRef, error)

	// RemoveImage removes a local image by ID or reference. No-op
	// if missing.
	RemoveImage(ctx context.Context, ref string) error

	// Capabilities advertises optional features this backend
	// supports. compose.Plan.Validate keys feature gates off this
	// struct so per-backend conditionals stay out of the validator.
	// The returned value should be a constant for the lifetime of
	// the Runtime; callers may cache it.
	Capabilities() Capabilities
}

// ImageRef identifies an image by digest and any associated tags.
type ImageRef struct {
	ID   string // sha256:... digest
	Tags []string
}

// Container is a minimal container handle returned by Run / Find /
// ListContainers. Use InspectContainer for fields not present here.
type Container struct {
	ID    string
	Name  string
	Image string
	State State

	// Labels are populated by ListContainers and FindContainerByLabel
	// when the backend can surface them cheaply. RunContainer may
	// leave this nil; callers that need labels after a fresh create
	// should InspectContainer. The compose orchestrator reads this
	// to identify the service name during reverse-topo teardown.
	Labels map[string]string
}

// State is the container lifecycle state per Docker Engine API.
type State string

const (
	StateCreated    State = "created"
	StateRunning    State = "running"
	StatePaused     State = "paused"
	StateRestarting State = "restarting"
	StateRemoving   State = "removing"
	StateExited     State = "exited"
	StateDead       State = "dead"
)

// ContainerDetails is the full inspected state of a container.
type ContainerDetails struct {
	Container
	Created   time.Time
	StartedAt time.Time
	User      string
	Env       []string // KEY=VALUE pairs from the running container
	Mounts    []MountInspect
	Labels    map[string]string

	// Security options as actually applied to the container, read back
	// from the backend's inspect (docker/podman HostConfig). Lets callers
	// verify that RunSpec.Privileged/CapAdd/SecurityOpt — including the
	// values merged from feature metadata onto compose services — landed
	// on the real container. Backends that don't surface these (e.g.
	// applecontainer) leave them at zero values.
	Privileged  bool
	CapAdd      []string
	SecurityOpt []string

	// ExitCode is the container's last exit code. Zero is ambiguous:
	// either "process is still running" or "process exited cleanly".
	// Use State to disambiguate (running vs exited).
	ExitCode int

	// FinishedAt is when the container's main process last exited. Zero
	// for never-exited containers.
	FinishedAt time.Time

	// Health reports the most recent HEALTHCHECK result.
	// HealthNone means the image declared no healthcheck (i.e. the
	// daemon never produced one); the compose orchestrator's
	// service_healthy gate treats this as "satisfied" so projects
	// without healthchecks still come up.
	Health HealthStatus
}

// HealthStatus mirrors docker's container health-check states.
// Backends that don't surface a typed health value report
// HealthNone, which the compose orchestrator interprets as
// "no healthcheck declared" — semantically equivalent to docker's
// default for images without a HEALTHCHECK directive.
type HealthStatus string

const (
	// HealthNone means the image / runtime did not surface a
	// healthcheck status. The orchestrator treats this as
	// satisfied (compose v2 behavior for healthcheck-less services).
	HealthNone HealthStatus = ""

	// HealthStarting is the daemon's transitional state — the
	// container is up but the healthcheck hasn't produced a verdict
	// yet. Orchestrator keeps polling.
	HealthStarting HealthStatus = "starting"

	// HealthHealthy means the most recent check passed.
	HealthHealthy HealthStatus = "healthy"

	// HealthUnhealthy means the most recent check failed. The
	// orchestrator surfaces this through *HealthTimeoutError if it
	// persists past the gate's deadline.
	HealthUnhealthy HealthStatus = "unhealthy"
)

// ImageDetails is the inspected state of a local image. Labels are
// the source of truth for the devcontainer.metadata pre-baked-image
// fast path.
type ImageDetails struct {
	ID     string
	Tags   []string
	Labels map[string]string
	Env    []string

	// User is the image's default USER directive (Config.User), or "" if
	// unset. Used to determine the effective container user for UID
	// reconciliation when devcontainer.json's remoteUser/containerUser
	// are also empty.
	User string

	// Entrypoint is the image's ENTRYPOINT (Config.Entrypoint), nil if
	// unset. Used to preserve the image's own entrypoint underneath the
	// feature-entrypoint wrapper when a feature declares an entrypoint
	// (e.g. docker-in-docker) and the consumer (compose service) declares
	// none of its own.
	Entrypoint []string
}

// MountInspect describes a mount as reported by the runtime, not what
// was requested. Differences (e.g. resolved volume name) reflect the
// daemon's view.
type MountInspect struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

// RunSpec is the input to RunContainer.
type RunSpec struct {
	Image      string
	Name       string
	Cmd        []string
	Entrypoint []string

	User       string
	WorkingDir string
	Env        map[string]string
	Labels     map[string]string

	Mounts []MountSpec

	// RunArgs is a list of additional Docker CLI-style flags (e.g.
	// "--add-host=host:ip"). Implementations parse and apply where
	// supported; unsupported flags become warnings, not errors.
	RunArgs []string

	Init        bool
	Privileged  bool
	CapAdd      []string
	SecurityOpt []string

	// HealthCheck declares the HEALTHCHECK directive at create time.
	// Nil means inherit from the image (i.e. no override). Used by
	// the compose orchestrator to translate `healthcheck:` directives.
	HealthCheck *HealthCheckSpec

	// Networks lists project networks the container joins. Empty
	// means "backend default" — docker assigns the default bridge;
	// apple assigns the built-in vmnet network. Used by the compose
	// orchestrator to attach services to the project network it
	// just created via CreateNetwork.
	Networks []string

	// Ports lists the ports this container publishes to the host.
	// Empty means no publishing (the container's ports are reachable
	// inside the project network but not from the host). Used by
	// the compose orchestrator to translate `ports:` directives.
	Ports []PortBinding

	// RestartPolicy controls whether the runtime restarts the
	// container on exit. Zero-value (RestartNo) matches docker's
	// `no` default. Used by the compose orchestrator to translate
	// `restart:` directives.
	RestartPolicy RestartPolicy

	// OverrideCommand, when true, forces Cmd to be ["/bin/sh","-c","while sleep 1000; do :; done"]
	// so the container stays alive for exec-based interaction. Spec default true.
	OverrideCommand bool

	// MemoryBytes is the hard memory limit for the container, in bytes.
	// Zero means "unset": the backend's own default applies — for docker
	// that's no cgroup limit; for apple it's the apiserver's per-VM
	// default (1 GiB on 0.12.x). Negative values are rejected by the
	// backend.
	//
	// On apple, this sizes the per-container VM at boot; the guest
	// kernel sees exactly this much memory and the value cannot be
	// resized without container recreation. On docker, this maps to
	// HostConfig.Memory and is enforced by cgroups.
	MemoryBytes int64

	// NanoCPUs is the CPU limit expressed in nano-units: 1_000_000_000
	// = one full CPU, 2_500_000_000 = 2.5 CPUs. Matches docker's
	// HostConfig.NanoCPUs convention so a single field works across
	// backends. Zero means "unset". Apple's apiserver takes an integer
	// CPU count, so the value is rounded up to the next whole CPU at
	// the bridge boundary (e.g. 1_500_000_000 → 2 cpus).
	NanoCPUs int64
}

// PortBinding describes a host->container port publish. Translates
// to docker's HostConfig.PortBindings + Config.ExposedPorts on the
// docker backend. Other backends translate where possible; an
// unsupported PortBinding on a backend that can't model it is
// the backend's choice to error or pass through.
type PortBinding struct {
	// HostIP optionally restricts the bind to a specific host
	// address. Empty = all interfaces (docker's 0.0.0.0 default).
	HostIP string

	// HostPort is the host-side port. Empty = let the daemon pick
	// (docker assigns from the ephemeral range).
	HostPort string

	// ContainerPort is the in-container port that's being
	// published. Required.
	ContainerPort int

	// Protocol is "tcp" or "udp". Empty defaults to "tcp".
	Protocol string
}

// HealthCheckSpec mirrors compose's healthcheck: directive plus
// docker's HEALTHCHECK config. Test is the command (with
// CMD/CMD-SHELL prefix as compose's HealthCheckTest already
// normalizes). Disable=true short-circuits to NONE, overriding any
// image-baked healthcheck.
type HealthCheckSpec struct {
	Test          []string
	Interval      time.Duration
	Timeout       time.Duration
	Retries       int
	StartPeriod   time.Duration
	StartInterval time.Duration
	Disable       bool
}

// RestartPolicy controls auto-restart behavior of a container.
// Mirrors docker compose's `restart:` directive values.
type RestartPolicy string

const (
	RestartNo            RestartPolicy = ""
	RestartAlways        RestartPolicy = "always"
	RestartOnFailure     RestartPolicy = "on-failure"
	RestartUnlessStopped RestartPolicy = "unless-stopped"
)

// MountSpec is a request for a single mount on a container.
type MountSpec struct {
	Type        MountType
	Source      string
	Target      string
	ReadOnly    bool
	Propagation string // host bind consistency: "consistent" | "cached" | "delegated" | "" (Linux default)
}

type MountType string

const (
	MountBind   MountType = "bind"
	MountVolume MountType = "volume"
	MountTmpfs  MountType = "tmpfs"
)

// BuildSpec is the input to BuildImage.
type BuildSpec struct {
	ContextPath string
	Dockerfile  string
	Tag         string
	Args        map[string]string
	Target      string
	CacheFrom   []string
	NoCache     bool
	Platform    string
}

// TtySize is a pty geometry in character cells. A zero value means
// "unspecified" — callers that don't care can leave it at the zero value
// and the runtime falls back to the container's default (typically 80x24).
type TtySize struct {
	Width  uint16
	Height uint16
}

// ExecOptions configures an exec invocation. If Stdout/Stderr are nil,
// captured output is returned in ExecResult; otherwise output streams
// directly and ExecResult's Stdout/Stderr are empty.
type ExecOptions struct {
	Cmd        []string
	Env        map[string]string
	User       string
	WorkingDir string
	Tty        bool
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer

	// InitialTtySize, when Tty is true and both dimensions are non-zero,
	// sets the pty geometry at exec creation. Ignored when Tty is false.
	InitialTtySize TtySize

	// ResizeCh, when non-nil and Tty is true, delivers pty geometry
	// updates for the lifetime of the exec. The runtime forwards each
	// received TtySize to the underlying exec session. The caller owns
	// the channel and MUST NOT close it before ExecContainer returns.
	// Sends are non-blocking from the caller's perspective: if the
	// runtime is mid-resize, the latest value may be coalesced. Ignored
	// when Tty is false.
	ResizeCh <-chan TtySize
}

// ExecResult is the outcome of ExecContainer.
type ExecResult struct {
	ExitCode int
	Stdout   string // only populated if ExecOptions.Stdout was nil
	Stderr   string // only populated if ExecOptions.Stderr was nil
}

// StopOptions configures StopContainer.
type StopOptions struct {
	Timeout time.Duration // grace period before SIGKILL; 0 = engine default
}

// RemoveOptions configures RemoveContainer.
type RemoveOptions struct {
	Force         bool
	RemoveVolumes bool
}

// BuildEvent is a streaming progress message emitted during BuildImage or
// PullImage. Most fields are kind-specific; consumers should switch on Kind.
type BuildEvent struct {
	Kind    BuildEventKind
	Message string
	LayerID string
	Digest  string
}

type BuildEventKind string

const (
	BuildEventLog          BuildEventKind = "log"
	BuildEventLayer        BuildEventKind = "layer"
	BuildEventCompleted    BuildEventKind = "completed"
	BuildEventPullProgress BuildEventKind = "pull_progress"
)
