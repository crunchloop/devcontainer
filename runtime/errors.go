package runtime

import (
	"errors"
	"fmt"
)

// ErrNotImplemented is returned by Runtime methods that have not yet
// been implemented for the chosen backend (e.g. BuildImage on a
// CLI-shim runtime in v1).
var ErrNotImplemented = errors.New("runtime: not implemented")

// ImageNotFoundError indicates the requested image is not present
// locally and could not be pulled.
type ImageNotFoundError struct {
	Ref string
	Err error
}

func (e *ImageNotFoundError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("image not found: %s: %v", e.Ref, e.Err)
	}
	return fmt.Sprintf("image not found: %s", e.Ref)
}

func (e *ImageNotFoundError) Unwrap() error { return e.Err }

// ContainerNotFoundError indicates a container with the given id or
// name does not exist.
type ContainerNotFoundError struct {
	ID  string
	Err error
}

func (e *ContainerNotFoundError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("container not found: %s: %v", e.ID, e.Err)
	}
	return fmt.Sprintf("container not found: %s", e.ID)
}

func (e *ContainerNotFoundError) Unwrap() error { return e.Err }

// ExecFailedError indicates an exec call completed with a non-zero
// exit code. Captured stderr is included for diagnostics.
type ExecFailedError struct {
	ExitCode int
	Stderr   string
}

func (e *ExecFailedError) Error() string {
	return fmt.Sprintf("exec failed (exit=%d): %s", e.ExitCode, e.Stderr)
}

// DaemonUnavailableError indicates the container engine daemon is
// unreachable (socket missing, permission denied, etc.).
type DaemonUnavailableError struct {
	Err error
}

func (e *DaemonUnavailableError) Error() string {
	return fmt.Sprintf("container daemon unavailable: %v", e.Err)
}

func (e *DaemonUnavailableError) Unwrap() error { return e.Err }
