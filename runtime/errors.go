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

// ComposeUnavailableError indicates the `docker compose` v2 plugin is
// not installed / not on PATH. Returned by ComposeRuntime methods on
// first attempted use; cached on the runtime so subsequent calls
// short-circuit with the same error.
type ComposeUnavailableError struct {
	Err error
}

func (e *ComposeUnavailableError) Error() string {
	return fmt.Sprintf("docker compose v2 unavailable (install Docker Compose v2): %v", e.Err)
}

func (e *ComposeUnavailableError) Unwrap() error { return e.Err }

// ComposeFailedError wraps a non-zero exit from `docker compose`,
// preserving the captured stderr for diagnostics. Distinct from
// ComposeUnavailableError, which means the binary itself isn't there.
type ComposeFailedError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *ComposeFailedError) Error() string {
	return fmt.Sprintf("docker compose failed (exit=%d): %s", e.ExitCode, e.Stderr)
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

// BuilderUnavailableError indicates the container engine's image-build
// component is missing or not running. Distinct from
// DaemonUnavailableError because the build engine is typically a
// separate process / VM that can be started independently (e.g.
// Apple's `container builder start`, Docker's BuildKit daemon).
type BuilderUnavailableError struct {
	// Hint is a backend-specific message telling the user how to
	// remediate (e.g. "run `container builder start`").
	Hint string
	Err  error
}

func (e *BuilderUnavailableError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("image build engine unavailable (%s): %v", e.Hint, e.Err)
	}
	return fmt.Sprintf("image build engine unavailable: %v", e.Err)
}

func (e *BuilderUnavailableError) Unwrap() error { return e.Err }
