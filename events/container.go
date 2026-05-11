package events

import "time"

const (
	TypeContainerCreating = "container.creating"
	TypeContainerCreated  = "container.created"
	TypeContainerStarted  = "container.started"
	TypeContainerStopped  = "container.stopped"
	TypeContainerRemoved  = "container.removed"
)

// ContainerCreatingEvent fires immediately before a CreateContainer call.
// Name is the resolved container name, or empty for compose-managed
// containers (compose generates the name itself).
type ContainerCreatingEvent struct {
	Base
	Name string
}

func (ContainerCreatingEvent) EventType() string { return TypeContainerCreating }

// ContainerCreatedEvent fires after CreateContainer succeeds.
type ContainerCreatedEvent struct {
	Base
	ContainerID string
	Name        string
}

func (ContainerCreatedEvent) EventType() string { return TypeContainerCreated }

// ContainerStartedEvent fires after StartContainer succeeds.
type ContainerStartedEvent struct {
	Base
	ContainerID string
	StartedAt   time.Time
}

func (ContainerStartedEvent) EventType() string { return TypeContainerStarted }

// ContainerStoppedEvent fires after StopContainer returns. ExitCode is the
// container's exit code if known; -1 if not retrieved.
type ContainerStoppedEvent struct {
	Base
	ContainerID string
	ExitCode    int
}

func (ContainerStoppedEvent) EventType() string { return TypeContainerStopped }

// ContainerRemovedEvent fires after RemoveContainer succeeds.
type ContainerRemovedEvent struct {
	Base
	ContainerID string
}

func (ContainerRemovedEvent) EventType() string { return TypeContainerRemoved }
