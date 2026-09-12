// Package activity records who did what to which machine.
//
// It backs the activity tab -- who attached when, who holds control, who held
// it before (design section 8) -- and it is also the audit trail section 10
// asks for, naming the identity the proxy asserted.
package activity

import (
	"log/slog"
	"sync"
	"time"
)

// Action types.
const (
	ActionAttach   = "attach"
	ActionDetach   = "detach"
	ActionGranted  = "control-granted"
	ActionReleased = "control-released"
	ActionExpired  = "control-expired"
	ActionPower    = "power"
	ActionDenied   = "denied"
)

// Event is one thing that happened to a machine.
type Event struct {
	At      time.Time `json:"at"`
	Machine string    `json:"machine"`
	User    string    `json:"user"`
	Action  string    `json:"action"`
	Detail  string    `json:"detail,omitempty"`
	Channel string    `json:"channel,omitempty"`
}

// DefaultCapacity is how many events are kept per machine.
const DefaultCapacity = 200

// Log is a bounded, in-memory history.
//
// In memory only, deliberately: this is a debugging convenience, and the
// durable record is the structured log that every event is also written to,
// which lands in the journal where the rest of the hypervisor's history is.
type Log struct {
	capacity int
	log      *slog.Logger

	mu     sync.Mutex
	byID   map[string][]Event
	recent []Event
}

// New returns an activity log.
func New(capacity int, log *slog.Logger) *Log {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if log == nil {
		log = slog.Default()
	}
	return &Log{capacity: capacity, log: log, byID: make(map[string][]Event)}
}

// Record appends an event and mirrors it to the structured log.
func (l *Log) Record(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}

	l.mu.Lock()
	l.byID[ev.Machine] = appendBounded(l.byID[ev.Machine], ev, l.capacity)
	l.recent = appendBounded(l.recent, ev, l.capacity*2)
	l.mu.Unlock()

	l.log.Info("activity",
		"machine", ev.Machine, "user", ev.User, "action", ev.Action,
		"channel", ev.Channel, "detail", ev.Detail)
}

func appendBounded(s []Event, ev Event, capacity int) []Event {
	s = append(s, ev)
	if len(s) > capacity {
		// Copy rather than reslice, so the backing array does not grow
		// without bound behind a sliding window.
		out := make([]Event, capacity)
		copy(out, s[len(s)-capacity:])
		return out
	}
	return s
}

// Machine returns a machine's history, newest first.
func (l *Log) Machine(id string) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return reversed(l.byID[id])
}

// Recent returns the whole lab's history, newest first.
func (l *Log) Recent() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return reversed(l.recent)
}

func reversed(in []Event) []Event {
	out := make([]Event, len(in))
	for i, ev := range in {
		out[len(in)-1-i] = ev
	}
	return out
}
