package main

import (
	"fmt"
	"io"
	"os"

	"github.com/crunchloop/devcontainer/events"
)

// startEventPrinter spawns a goroutine that drains ch to stderr in
// human-readable form and returns the channel for callers to pass to
// the engine, plus a stop function that closes the channel and waits
// for the drainer to finish. The returned channel must not be closed
// by the caller — stop() does that.
func startEventPrinter() (chan events.Event, func()) {
	ch := make(chan events.Event, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			printEvent(os.Stderr, ev)
		}
	}()
	return ch, func() {
		close(ch)
		<-done
	}
}

// printEvent renders an engine event as a single human-readable line.
// Events without a meaningful textual representation print their type tag.
func printEvent(w io.Writer, ev events.Event) {
	switch e := ev.(type) {
	case events.ConfigWarningEvent:
		fmt.Fprintf(w, "[warn] %s: %s\n", e.Code, e.Message)
	case events.WarnEvent:
		fmt.Fprintf(w, "[warn] %s\n", e.Message)
	case events.LifecycleStartEvent:
		fmt.Fprintf(w, "[lifecycle] %s starting\n", e.Phase)
	case events.LifecycleOutputEvent:
		fmt.Fprintf(w, "[%s] %s", e.Phase, e.Line)
	case events.LifecycleCompletedEvent:
		fmt.Fprintf(w, "[lifecycle] %s done\n", e.Phase)
	case events.LifecycleSkippedEvent:
		fmt.Fprintf(w, "[lifecycle] %s skipped: %s\n", e.Phase, e.Reason)
	case events.BuildStartEvent:
		fmt.Fprintf(w, "[build] start\n")
	case events.BuildLogEvent:
		fmt.Fprint(w, e.Line)
	case events.BuildCompletedEvent:
		fmt.Fprintf(w, "[build] done: %s\n", e.ImageID)
	case events.FeatureResolveStartEvent:
		fmt.Fprintf(w, "[feature] resolving %s\n", e.Ref)
	case events.FeatureResolvedEvent:
		if e.FromCache {
			fmt.Fprintf(w, "[feature] %s (cached)\n", e.Ref)
		} else {
			fmt.Fprintf(w, "[feature] %s\n", e.Ref)
		}
	case events.ContainerCreatingEvent:
		fmt.Fprintf(w, "[container] creating\n")
	case events.ContainerCreatedEvent:
		fmt.Fprintf(w, "[container] created: %s\n", e.ContainerID)
	case events.ContainerStartedEvent:
		fmt.Fprintf(w, "[container] started\n")
	case events.ContainerStoppedEvent:
		fmt.Fprintf(w, "[container] stopped: %s\n", e.ContainerID)
	case events.ContainerRemovedEvent:
		fmt.Fprintf(w, "[container] removed: %s\n", e.ContainerID)
	default:
		fmt.Fprintf(w, "[%s]\n", ev.EventType())
	}
}
