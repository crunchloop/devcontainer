package events

const (
	TypeLifecycleStart     = "lifecycle.start"
	TypeLifecycleOutput    = "lifecycle.output"
	TypeLifecycleSkipped   = "lifecycle.skipped"
	TypeLifecycleCompleted = "lifecycle.completed"
)

// LifecycleStartEvent fires when a phase begins executing in the container.
// Phase is one of: "initialize", "onCreate", "updateContent", "postCreate",
// "postStart", "postAttach". Command is the resolved shell or argv string
// for display (may be the joined parallel form).
type LifecycleStartEvent struct {
	Base
	Phase   string
	Command string
}

func (LifecycleStartEvent) EventType() string { return TypeLifecycleStart }

// LifecycleOutputEvent carries one line of a lifecycle command's stdout or
// stderr. Emitted as the command runs.
type LifecycleOutputEvent struct {
	Base
	Phase  string
	Stream string // "stdout" or "stderr"
	Line   string
}

func (LifecycleOutputEvent) EventType() string { return TypeLifecycleOutput }

// LifecycleSkippedEvent fires when a phase is skipped because its
// idempotency marker is already present. Reason is a short tag like
// "marker_present".
type LifecycleSkippedEvent struct {
	Base
	Phase  string
	Reason string
}

func (LifecycleSkippedEvent) EventType() string { return TypeLifecycleSkipped }

// LifecycleCompletedEvent fires when a phase finishes. ExitCode is the
// process exit; 0 on success.
type LifecycleCompletedEvent struct {
	Base
	Phase      string
	ExitCode   int
	DurationMs int64
}

func (LifecycleCompletedEvent) EventType() string { return TypeLifecycleCompleted }
