package events

import (
	"sync/atomic"
	"time"
)

// Event is the marker interface for every event type. All concrete events
// embed Base so they inherit Seq() and Time() for free; they implement
// EventType() with a string constant.
type Event interface {
	EventType() string
	Seq() uint64
	Time() time.Time
}

// Base carries the fields common to every event. Embed it in each concrete
// event type.
type Base struct {
	seq  uint64
	time time.Time
}

func (b Base) Seq() uint64     { return b.seq }
func (b Base) Time() time.Time { return b.time }

// Emitter allocates monotonic sequence numbers and stamps Base. One
// Emitter per Engine; safe for concurrent use.
type Emitter struct {
	counter atomic.Uint64
	now     func() time.Time
}

// NewEmitter returns an Emitter with the given clock (nil = time.Now).
func NewEmitter(now func() time.Time) *Emitter {
	if now == nil {
		now = time.Now
	}
	return &Emitter{now: now}
}

// Stamp returns a Base with a fresh Seq and current Time.
func (e *Emitter) Stamp() Base {
	return Base{
		seq:  e.counter.Add(1),
		time: e.now(),
	}
}

// Send writes ev to ch if non-nil and not full. Never blocks. Returns
// true iff the event was accepted.
func Send(ch chan<- Event, ev Event) bool {
	if ch == nil {
		return false
	}
	select {
	case ch <- ev:
		return true
	default:
		return false
	}
}
