package events

import "github.com/crunchloop/devcontainer/config"

const (
	TypeConfigResolved = "config.resolved"
	TypeConfigWarning  = "config.warning"
)

// ConfigResolvedEvent fires once per Engine.Up after Resolve returns, before
// any image work. Config is the same pointer the caller would observe via
// Workspace.Config later — consumers must treat it as read-only.
type ConfigResolvedEvent struct {
	Base
	Config *config.ResolvedConfig
}

func (ConfigResolvedEvent) EventType() string { return TypeConfigResolved }

// ConfigWarningEvent fires once per entry in cfg.Warnings (unknown field,
// deprecated key, substitution miss, etc.).
type ConfigWarningEvent struct {
	Base
	Code    string
	Message string
}

func (ConfigWarningEvent) EventType() string { return TypeConfigWarning }
