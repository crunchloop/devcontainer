package devcontainer

import (
	"context"
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/events"
)

// TestUpEmitsEvents verifies that an Up against the fake runtime drives the
// full event surface relevant to image-source: ConfigResolvedEvent fires,
// BuildStartEvent is emitted before the pull, and container creating /
// created / started arrive in monotonic Seq order with non-zero Time.
func TestUpEmitsEvents(t *testing.T) {
	rt := newFakeRuntime()
	eng, err := New(EngineOptions{Runtime: rt})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)

	ch := make(chan events.Event, 64)
	_, err = eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		Events:               ch,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	close(ch)

	seen := map[string]int{}
	var lastSeq uint64
	for ev := range ch {
		seen[ev.EventType()]++
		if ev.Seq() <= lastSeq {
			t.Errorf("non-monotonic Seq: %d after %d (event %s)", ev.Seq(), lastSeq, ev.EventType())
		}
		lastSeq = ev.Seq()
		if ev.Time().IsZero() {
			t.Errorf("event %s missing Time", ev.EventType())
		}
	}

	for _, want := range []string{
		events.TypeConfigResolved,
		events.TypeBuildStart,
		events.TypeContainerCreating,
		events.TypeContainerCreated,
		events.TypeContainerStarted,
	} {
		if seen[want] == 0 {
			t.Errorf("missing event type %q (got %v)", want, seen)
		}
	}
}

// TestExecEmitsEvents verifies the opt-in ExecOptions.EmitEvents path.
func TestExecEmitsEvents(t *testing.T) {
	rt := newFakeRuntime()
	eng, err := New(EngineOptions{Runtime: rt})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wsDir := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)
	ws, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: wsDir})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	ch := make(chan events.Event, 8)
	_, err = eng.Exec(context.Background(), ws, ExecOptions{
		Cmd:        []string{"true"},
		EmitEvents: true,
		Events:     ch,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	close(ch)

	var start, done bool
	for ev := range ch {
		switch ev.(type) {
		case events.ExecStartEvent:
			start = true
		case events.ExecCompletedEvent:
			done = true
		}
	}
	if !start || !done {
		t.Fatalf("want ExecStart+ExecCompleted, got start=%v done=%v", start, done)
	}
}

// TestExecEventsOptOutByDefault confirms that without EmitEvents, no exec
// events reach the channel even if Events is supplied.
func TestExecEventsOptOutByDefault(t *testing.T) {
	rt := newFakeRuntime()
	eng, err := New(EngineOptions{Runtime: rt})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wsDir := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)
	ws, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: wsDir})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	ch := make(chan events.Event, 8)
	_, err = eng.Exec(context.Background(), ws, ExecOptions{
		Cmd:    []string{"true"},
		Events: ch,
		// EmitEvents: false (default)
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("expected no events, got %T", ev)
	case <-time.After(50 * time.Millisecond):
	}
}
