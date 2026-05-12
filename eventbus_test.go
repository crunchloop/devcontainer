package devcontainer

import (
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/runtime"
)

// TestBuildCompletedEvent_DurationMs verifies that the bus stamps a real
// wall-clock duration on BuildCompletedEvent. Regression guard for the
// case where DurationMs was always 0 because the field was never set in
// translateBuildEvent.
func TestBuildCompletedEvent_DurationMs(t *testing.T) {
	out := make(chan events.Event, 16)
	bus := newEventBus(events.NewEmitter(nil), out)

	ch := bus.BuildChan(events.BuildSourceUIDReconcile)
	time.Sleep(20 * time.Millisecond)
	ch <- runtime.BuildEvent{Kind: runtime.BuildEventCompleted, Digest: "sha256:abc"}
	bus.Close()
	close(out)

	var done *events.BuildCompletedEvent
	for ev := range out {
		if e, ok := ev.(events.BuildCompletedEvent); ok {
			done = &e
		}
	}
	if done == nil {
		t.Fatal("no BuildCompletedEvent emitted")
	}
	if done.DurationMs < 15 {
		t.Errorf("DurationMs = %d, want >= 15 (slept 20ms before completion)", done.DurationMs)
	}
	if done.Source != events.BuildSourceUIDReconcile {
		t.Errorf("Source = %q, want %q", done.Source, events.BuildSourceUIDReconcile)
	}
}

// TestBuildCompletedEvent_DurationMs_ResetsPerSource verifies that
// successive BuildChan(source) calls on the same bus measure each build
// independently: the second build's duration is not inflated by the
// first build's elapsed time.
func TestBuildCompletedEvent_DurationMs_ResetsPerSource(t *testing.T) {
	out := make(chan events.Event, 32)
	bus := newEventBus(events.NewEmitter(nil), out)

	ch1 := bus.BuildChan(events.BuildSourceDockerfile)
	time.Sleep(50 * time.Millisecond)
	ch1 <- runtime.BuildEvent{Kind: runtime.BuildEventCompleted, Digest: "sha256:first"}

	// Drain so the translator has processed the first completion before
	// we reset the start timer for the second build.
	waitForCompleted(t, out, "sha256:first")

	bus.BuildChan(events.BuildSourceFeatures)
	time.Sleep(10 * time.Millisecond)
	ch1 <- runtime.BuildEvent{Kind: runtime.BuildEventCompleted, Digest: "sha256:second"}
	bus.Close()
	close(out)

	second := drainCompleted(out, "sha256:second")
	if second == nil {
		t.Fatal("no second BuildCompletedEvent emitted")
	}
	if second.DurationMs >= 50 {
		t.Errorf("second DurationMs = %d, expected < 50 (start should have reset)", second.DurationMs)
	}
	if second.Source != events.BuildSourceFeatures {
		t.Errorf("second Source = %q, want %q", second.Source, events.BuildSourceFeatures)
	}
}

func waitForCompleted(t *testing.T, out chan events.Event, digest string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-out:
			if e, ok := ev.(events.BuildCompletedEvent); ok && e.ImageID == digest {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for BuildCompletedEvent %s", digest)
		}
	}
}

func drainCompleted(out chan events.Event, digest string) *events.BuildCompletedEvent {
	for ev := range out {
		if e, ok := ev.(events.BuildCompletedEvent); ok && e.ImageID == digest {
			return &e
		}
	}
	return nil
}
