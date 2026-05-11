package events

const TypeWarn = "engine.warn"

// WarnEvent is the catch-all surface for engine-level diagnostics that
// don't fit a more specific event. Code is a short stable tag (e.g.
// "compose_ports_ignored"); Message is human-readable detail.
type WarnEvent struct {
	Base
	Code    string
	Message string
}

func (WarnEvent) EventType() string { return TypeWarn }
