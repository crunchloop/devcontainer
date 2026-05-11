package events

const (
	TypeExecStart     = "exec.start"
	TypeExecCompleted = "exec.completed"
)

// ExecStartEvent fires when an Engine.Exec begins. Opt-in via
// ExecOptions.EmitEvents: hot-loop readiness probes can run hundreds of
// execs per minute and would otherwise drown the events channel.
type ExecStartEvent struct {
	Base
	ContainerID string
	Cmd         []string
}

func (ExecStartEvent) EventType() string { return TypeExecStart }

// ExecCompletedEvent fires when an Engine.Exec returns. Opt-in via
// ExecOptions.EmitEvents.
type ExecCompletedEvent struct {
	Base
	ContainerID string
	ExitCode    int
	DurationMs  int64
}

func (ExecCompletedEvent) EventType() string { return TypeExecCompleted }
