package events

import (
	"sync"
	"testing"
	"time"
)

func TestEmitterStampMonotonic(t *testing.T) {
	em := NewEmitter(nil)
	a := em.Stamp()
	b := em.Stamp()
	c := em.Stamp()
	if a.Seq() != 1 || b.Seq() != 2 || c.Seq() != 3 {
		t.Fatalf("want 1,2,3; got %d,%d,%d", a.Seq(), b.Seq(), c.Seq())
	}
	if a.Time().IsZero() {
		t.Fatalf("Base.Time should be set")
	}
}

func TestEmitterStampConcurrent(t *testing.T) {
	em := NewEmitter(nil)
	const n = 200
	seen := make([]uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seen[i] = em.Stamp().Seq()
		}(i)
	}
	wg.Wait()

	bits := make(map[uint64]bool, n)
	var max uint64
	for _, s := range seen {
		if bits[s] {
			t.Fatalf("duplicate Seq %d", s)
		}
		bits[s] = true
		if s > max {
			max = s
		}
	}
	if max != n {
		t.Fatalf("max Seq=%d, want %d", max, n)
	}
}

func TestSendNonBlocking(t *testing.T) {
	em := NewEmitter(func() time.Time { return time.Unix(0, 0) })
	ch := make(chan Event, 1)
	ev := WarnEvent{Base: em.Stamp(), Code: "x"}
	if !Send(ch, ev) {
		t.Fatalf("first send should succeed")
	}
	if Send(ch, ev) {
		t.Fatalf("second send should drop (full)")
	}
}

func TestSendNilChan(t *testing.T) {
	if Send(nil, WarnEvent{}) {
		t.Fatalf("send to nil channel should return false")
	}
}

func TestEventTypes(t *testing.T) {
	cases := []struct {
		ev   Event
		want string
	}{
		{ConfigResolvedEvent{}, TypeConfigResolved},
		{ConfigWarningEvent{}, TypeConfigWarning},
		{FeatureResolveStartEvent{}, TypeFeatureResolveStart},
		{FeatureResolvedEvent{}, TypeFeatureResolved},
		{FeatureSkippedEvent{}, TypeFeatureSkipped},
		{BuildStartEvent{}, TypeBuildStart},
		{BuildLogEvent{}, TypeBuildLog},
		{BuildLayerEvent{}, TypeBuildLayer},
		{BuildCompletedEvent{}, TypeBuildCompleted},
		{ContainerCreatingEvent{}, TypeContainerCreating},
		{ContainerCreatedEvent{}, TypeContainerCreated},
		{ContainerStartedEvent{}, TypeContainerStarted},
		{ContainerStoppedEvent{}, TypeContainerStopped},
		{ContainerRemovedEvent{}, TypeContainerRemoved},
		{LifecycleStartEvent{}, TypeLifecycleStart},
		{LifecycleOutputEvent{}, TypeLifecycleOutput},
		{LifecycleSkippedEvent{}, TypeLifecycleSkipped},
		{LifecycleCompletedEvent{}, TypeLifecycleCompleted},
		{ExecStartEvent{}, TypeExecStart},
		{ExecCompletedEvent{}, TypeExecCompleted},
		{WarnEvent{}, TypeWarn},
	}
	for _, c := range cases {
		if got := c.ev.EventType(); got != c.want {
			t.Errorf("%T.EventType()=%q, want %q", c.ev, got, c.want)
		}
	}
}
