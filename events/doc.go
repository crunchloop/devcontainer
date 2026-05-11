// Package events defines the structured event surface emitted by Engine
// operations (Up / Down / Exec / RunLifecycle) and feature/build/runtime
// activity reached through them.
//
// EXPERIMENTAL: This package is doc-tagged experimental until v1.0.0.
// Field names, event types, and emission cadence may change without a
// SemVer-major bump while the library is below v1. Callers should write
// dispatch code defensively (type switch with default branch) and not
// rely on Seq gaps being absent across versions.
//
// Usage:
//
//	ch := make(chan events.Event, 64)
//	go func() {
//	    for ev := range ch {
//	        switch e := ev.(type) {
//	        case events.ConfigResolvedEvent:
//	            // ...
//	        case events.LifecycleCompletedEvent:
//	            // ...
//	        }
//	    }
//	}()
//	ws, err := eng.Up(ctx, devcontainer.UpOptions{Events: ch, ...})
//
// All events carry a monotonic Seq (allocated per Engine) and a Time
// stamped at emission. The channel is non-blocking from the engine's
// perspective — events are dropped on a full channel rather than
// stalling work.
//
// Channel ownership: the caller owns the channel; the engine only writes
// to it and never closes it. The caller MUST NOT close the channel
// before the operation (Up / Down / Exec) it was passed to returns —
// doing so races with the engine's send and will panic. The safe
// pattern is to close after the call returns, or simply leave the
// channel open and let it be garbage-collected.
package events
