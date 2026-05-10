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
}

// ImageRef identifies an image by digest and any associated tags.
type ImageRef struct {
	ID   string // sha256:... digest
	Tags []string
}

// Container is a minimal container handle returned by Run / Find.
// Use InspectContainer for full details.
type Container struct {
	ID    string
	Name  string
	Image string
	State State
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

	// ExitCode is the container's last exit code. Zero is ambiguous:
	// either "process is still running" or "process exited cleanly".
	// Use State to disambiguate (running vs exited).
	ExitCode int

	// FinishedAt is when the container's main process last exited. Zero
	// for never-exited containers.
	FinishedAt time.Time
}

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

	// OverrideCommand, when true, forces Cmd to be ["/bin/sh","-c","while sleep 1000; do :; done"]
	// so the container stays alive for exec-based interaction. Spec default true.
	OverrideCommand bool
}

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
