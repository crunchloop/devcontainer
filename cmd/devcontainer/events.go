package main

import (
	"fmt"
	"io"
	"os"

	"github.com/crunchloop/devcontainer/events"
)

// stderrf writes a formatted message to stderr, ignoring write errors —
// there's nothing reasonable to do if writing to stderr itself fails.
func stderrf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format, args...)
}

// outf is the io.Writer-targeted variant used by printEvent (the writer
// is injectable so tests can substitute a buffer).
func outf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// logLevel controls how much of the engine's event stream printEvent
// renders. Higher levels are noisier.
type logLevel int

const (
	levelInfo  logLevel = iota // default: progress + warnings + state changes
	levelDebug                 // + raw build logs + raw lifecycle stdout/stderr
	levelTrace                 // currently identical to debug; reserved
)

// startEventPrinter spawns a goroutine that drains ch to stderr in
// human-readable form and returns the channel for callers to pass to
// the engine, plus a stop function that closes the channel and waits
// for the drainer to finish. The returned channel must not be closed
// by the caller — stop() does that.
func startEventPrinter(level logLevel) (chan events.Event, func()) {
	ch := make(chan events.Event, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			printEvent(os.Stderr, ev, level)
		}
	}()
	return ch, func() {
		close(ch)
		<-done
	}
}

// printEvent renders an engine event as a single human-readable line.
// Events without a meaningful textual representation print their type tag.
// Raw-output events (build logs, lifecycle stdout/stderr) are gated
// behind debug — they're useful for debugging but drown the progress
// stream at info.
func printEvent(w io.Writer, ev events.Event, level logLevel) {
	switch e := ev.(type) {
	case events.ConfigWarningEvent:
		outf(w, "[warn] %s: %s\n", e.Code, e.Message)
	case events.WarnEvent:
		outf(w, "[warn] %s\n", e.Message)
	case events.LifecycleStartEvent:
		outf(w, "[lifecycle] %s starting\n", e.Phase)
	case events.LifecycleOutputEvent:
		if level >= levelDebug {
			outf(w, "[%s] %s", e.Phase, e.Line)
		}
	case events.LifecycleCompletedEvent:
		outf(w, "[lifecycle] %s done\n", e.Phase)
	case events.LifecycleSkippedEvent:
		outf(w, "[lifecycle] %s skipped: %s\n", e.Phase, e.Reason)
	case events.BuildStartEvent:
		outf(w, "[build] start\n")
	case events.BuildLogEvent:
		if level >= levelDebug {
			_, _ = fmt.Fprint(w, e.Line)
		}
	case events.BuildCompletedEvent:
		outf(w, "[build] done: %s\n", e.ImageID)
	case events.FeatureResolveStartEvent:
		outf(w, "[feature] resolving %s\n", e.Ref)
	case events.FeatureResolvedEvent:
		if e.FromCache {
			outf(w, "[feature] %s (cached)\n", e.Ref)
		} else {
			outf(w, "[feature] %s\n", e.Ref)
		}
	case events.ContainerCreatingEvent:
		outf(w, "[container] creating\n")
	case events.ContainerCreatedEvent:
		outf(w, "[container] created: %s\n", e.ContainerID)
	case events.ContainerStartedEvent:
		outf(w, "[container] started\n")
	case events.ContainerStoppedEvent:
		outf(w, "[container] stopped: %s\n", e.ContainerID)
	case events.ContainerRemovedEvent:
		outf(w, "[container] removed: %s\n", e.ContainerID)
	default:
		outf(w, "[%s]\n", ev.EventType())
	}
}
