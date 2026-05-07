package devcontainer

import (
	"context"
	"errors"
	"fmt"

	"github.com/crunchloop/devcontainer/runtime"
)

// Engine drives the devcontainer lifecycle on top of a Runtime.
type Engine struct {
	runtime runtime.Runtime
	opts    EngineOptions
}

// EngineOptions configures a new Engine.
type EngineOptions struct {
	// Runtime is the container backend. Required.
	Runtime runtime.Runtime
}

// New constructs an Engine. Returns an error if Runtime is nil.
func New(opts EngineOptions) (*Engine, error) {
	if opts.Runtime == nil {
		return nil, errors.New("EngineOptions.Runtime is required")
	}
	return &Engine{runtime: opts.Runtime, opts: opts}, nil
}

// Common labels written to every container the engine creates. Labels are
// the source of truth for container ↔ workspace mapping; container names
// are deterministic but not relied upon for lookup.
const (
	LabelDevcontainerID       = "dev.containers.id"
	LabelLocalWorkspaceFolder = "dev.containers.localWorkspaceFolder"
	LabelConfigPath           = "dev.containers.configPath"
	LabelEngine               = "dev.containers.engine"

	engineIdent = "devcontainer-go/0.1"
)

// containerName returns the deterministic container name for a workspace id.
func containerName(id WorkspaceID) string {
	return "devcontainer-" + string(id)
}

// errBuildSourceNotImplemented and errComposeSourceNotImplemented are returned
// by Engine.Up for source kinds whose runtime path lands in later milestones.
var (
	errBuildSourceNotImplemented   = fmt.Errorf("build source: %w", runtime.ErrNotImplemented)
	errComposeSourceNotImplemented = fmt.Errorf("compose source: %w", runtime.ErrNotImplemented)
)

// ctxIfDone returns ctx.Err() if ctx is cancelled, nil otherwise. Used at
// the entry of every public Engine method so that a cancelled ctx never
// triggers a daemon round-trip.
func ctxIfDone(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
