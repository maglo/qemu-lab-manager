// Package telemetry emits cluster events as JSON
// (docs/design/cluster.md section 6).
//
// v1 writes one JSON object per line to an API stream and to stdout.
// Scenario events use KindScenario. OTLP is a later sink.
package telemetry

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Kinds.
const (
	KindNode     = "node"
	KindMachine  = "machine"
	KindScenario = "scenario"
)

// Types.
const (
	TypeUp           = "up"
	TypeCreated      = "created"
	TypeDestroyed    = "destroyed"
	TypePower        = "power"
	TypeLease        = "lease"
	TypeDenied       = "denied"
	TypeAttach       = "attach"
	TypeDetach       = "detach"
	TypePeerFail     = "peer-fail"
	TypeRunStarted   = "run.started"
	TypeRunFinished  = "run.finished"
	TypeStepStarted  = "step.started"
	TypeStepFinished = "step.finished"
)

// Event is one telemetry record.
type Event struct {
	At      time.Time      `json:"at"`
	Kind    string         `json:"kind"`
	Type    string         `json:"type"`
	Machine string         `json:"machine,omitempty"`
	User    string         `json:"user,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// DefaultCapacity is how many events the ring keeps for late subscribers.
const DefaultCapacity = 500

// Bus is a fan-out of telemetry events.
type Bus struct {
	capacity int
	stdout   io.Writer

	mu   sync.Mutex
	ring []Event
	subs map[chan Event]struct{}
}

// Options configures a Bus.
type Options struct {
	Capacity int
	// Stdout receives one JSON object per line. Nil disables the mirror.
	// Pass os.Stdout on a node so the unit journal holds the stream.
	Stdout io.Writer
}

// New returns a Bus.
func New(opts Options) *Bus {
	if opts.Capacity <= 0 {
		opts.Capacity = DefaultCapacity
	}
	return &Bus{
		capacity: opts.Capacity,
		stdout:   opts.Stdout,
		subs:     make(map[chan Event]struct{}),
	}
}

// Emit publishes an event to the ring, subscribers and stdout.
func (b *Bus) Emit(ev Event) {
	if b == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}

	b.mu.Lock()
	b.ring = append(b.ring, ev)
	if len(b.ring) > b.capacity {
		out := make([]Event, b.capacity)
		copy(out, b.ring[len(b.ring)-b.capacity:])
		b.ring = out
	}
	subs := make([]chan Event, 0, len(b.subs))
	for ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	if b.stdout != nil {
		enc := json.NewEncoder(b.stdout)
		_ = enc.Encode(ev)
	}
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// Slow subscriber: drop rather than block the lab.
		}
	}
}

// Snapshot returns a copy of the ring, oldest first.
func (b *Bus) Snapshot() []Event {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, len(b.ring))
	copy(out, b.ring)
	return out
}

// Subscribe receives live events. The channel is buffered; overflow drops.
// Call Unsubscribe when done.
func (b *Bus) Subscribe(buffer int) <-chan Event {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Event, buffer)
	if b == nil {
		close(ch)
		return ch
	}
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe stops delivery to ch and closes it.
func (b *Bus) Unsubscribe(ch <-chan Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.subs {
		if c == ch {
			delete(b.subs, c)
			close(c)
			return
		}
	}
}

// ObserveActivity mirrors an activity action into telemetry. Used so the
// activity log stays the audit tab and telemetry stays the stream.
func (b *Bus) ObserveActivity(machine, user, action, detail string) {
	if b == nil {
		return
	}
	ev := Event{
		Kind:    KindMachine,
		Machine: machine,
		User:    user,
		Detail:  detail,
	}
	switch action {
	case "created":
		ev.Type = TypeCreated
	case "destroyed":
		ev.Type = TypeDestroyed
	case "power":
		ev.Type = TypePower
	case "control-granted", "control-released", "control-expired":
		ev.Type = TypeLease
		ev.Detail = action
		if detail != "" {
			ev.Detail = action + ": " + detail
		}
	case "denied":
		ev.Type = TypeDenied
	case "attach":
		ev.Type = TypeAttach
	case "detach":
		ev.Type = TypeDetach
	default:
		ev.Type = action
	}
	b.Emit(ev)
}

// NodeUp emits the startup record.
func (b *Bus) NodeUp(machines int, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["machines"] = machines
	b.Emit(Event{Kind: KindNode, Type: TypeUp, Fields: fields})
}

// PeerFail emits a peer listing failure.
func (b *Bus) PeerFail(peerName, errText string) {
	b.Emit(Event{
		Kind:   KindNode,
		Type:   TypePeerFail,
		Detail: peerName + ": " + errText,
		Fields: map[string]any{"peer": peerName},
	})
}

// DefaultStdout is os.Stdout for production wiring.
func DefaultStdout() io.Writer { return os.Stdout }
