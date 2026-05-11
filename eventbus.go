package devcontainer

import (
	"sync"

	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/runtime"
)

// eventBus is the per-operation bridge between the caller's events.Event
// channel and the internal runtime.BuildEvent channel that runtime methods
// expect. It also stamps every emitted event with a fresh Seq/Time via the
// engine's Emitter.
//
// A nil bus is valid and silently discards all emissions and build events;
// callers don't need to nil-check before Emit / BuildChan.
type eventBus struct {
	emitter *events.Emitter
	out     chan<- events.Event

	mu       sync.Mutex
	buildCh  chan runtime.BuildEvent // lazily started
	buildSrc events.BuildSource      // current build source, attached to translated events
	wg       sync.WaitGroup          // build translator goroutine
	closed   bool
}

func newEventBus(emitter *events.Emitter, out chan<- events.Event) *eventBus {
	return &eventBus{emitter: emitter, out: out}
}

// Emit stamps ev's Base via the engine emitter and forwards it to the
// caller channel (non-blocking; drops if full or out == nil).
func (b *eventBus) Emit(ev events.Event) {
	if b == nil || b.out == nil {
		return
	}
	stamped, ok := withStamp(ev, b.emitter.Stamp())
	if !ok {
		return
	}
	events.Send(b.out, stamped)
}

// BuildChan returns a channel that runtime methods can write
// runtime.BuildEvent into. The translator goroutine reads from it, maps
// each event to the events.Build* shape with the given source, and emits.
//
// The same channel is returned across calls — callers shouldn't close it
// themselves. Call Close at the end of the operation to drain.
//
// If out is nil, returns nil — runtime methods skip the channel write.
func (b *eventBus) BuildChan(source events.BuildSource) chan<- runtime.BuildEvent {
	if b == nil || b.out == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buildSrc = source
	if b.buildCh != nil {
		return b.buildCh
	}
	b.buildCh = make(chan runtime.BuildEvent, 64)
	b.wg.Add(1)
	go b.translateLoop(b.buildCh)
	return b.buildCh
}

func (b *eventBus) translateLoop(in <-chan runtime.BuildEvent) {
	defer b.wg.Done()
	for be := range in {
		b.mu.Lock()
		src := b.buildSrc
		b.mu.Unlock()
		b.Emit(translateBuildEvent(be, src))
	}
}

// Close flushes the build translator and stops accepting further events.
// Safe to call from a defer.
func (b *eventBus) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	ch := b.buildCh
	b.mu.Unlock()
	if ch != nil {
		close(ch)
		b.wg.Wait()
	}
}

// translateBuildEvent maps a runtime.BuildEvent (which has no embedded
// Base) into the appropriate events.Build* type. The translator does not
// stamp Base; Emit handles that.
func translateBuildEvent(be runtime.BuildEvent, src events.BuildSource) events.Event {
	switch be.Kind {
	case runtime.BuildEventLayer, runtime.BuildEventPullProgress:
		return events.BuildLayerEvent{LayerID: be.LayerID, Status: be.Message}
	case runtime.BuildEventCompleted:
		return events.BuildCompletedEvent{ImageID: be.Digest}
	default:
		return events.BuildLogEvent{Stream: "stdout", Line: be.Message}
	}
}

// withStamp clones ev with the given Base. Returns (ev, false) if ev's
// concrete type is not one of the known events package types. This is a
// switch rather than reflection to keep the hot path allocation-free and
// to enforce that every event type is intentionally surfaced through the
// bus.
func withStamp(ev events.Event, b events.Base) (events.Event, bool) {
	switch e := ev.(type) {
	case events.ConfigResolvedEvent:
		e.Base = b
		return e, true
	case events.ConfigWarningEvent:
		e.Base = b
		return e, true
	case events.FeatureResolveStartEvent:
		e.Base = b
		return e, true
	case events.FeatureResolvedEvent:
		e.Base = b
		return e, true
	case events.FeatureSkippedEvent:
		e.Base = b
		return e, true
	case events.BuildStartEvent:
		e.Base = b
		return e, true
	case events.BuildLogEvent:
		e.Base = b
		return e, true
	case events.BuildLayerEvent:
		e.Base = b
		return e, true
	case events.BuildCompletedEvent:
		e.Base = b
		return e, true
	case events.ContainerCreatingEvent:
		e.Base = b
		return e, true
	case events.ContainerCreatedEvent:
		e.Base = b
		return e, true
	case events.ContainerStartedEvent:
		e.Base = b
		return e, true
	case events.ContainerStoppedEvent:
		e.Base = b
		return e, true
	case events.ContainerRemovedEvent:
		e.Base = b
		return e, true
	case events.LifecycleStartEvent:
		e.Base = b
		return e, true
	case events.LifecycleOutputEvent:
		e.Base = b
		return e, true
	case events.LifecycleSkippedEvent:
		e.Base = b
		return e, true
	case events.LifecycleCompletedEvent:
		e.Base = b
		return e, true
	case events.ExecStartEvent:
		e.Base = b
		return e, true
	case events.ExecCompletedEvent:
		e.Base = b
		return e, true
	case events.WarnEvent:
		e.Base = b
		return e, true
	default:
		return ev, false
	}
}
