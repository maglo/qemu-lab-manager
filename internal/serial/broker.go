package serial

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// EventKind distinguishes the two things a serial connection carries.
//
// They must not share one stream: mixed together, a status message looks like
// something the VM printed -- it lands in the recording and in anything
// grepping the output. Websockets already carry two message types, so output
// goes in binary frames and status in text frames (design section 11).
type EventKind int

const (
	// EventOutput is what the VM printed. Raw bytes, binary frame.
	EventOutput EventKind = iota
	// EventStatus is the viewer talking about itself. JSON, text frame.
	EventStatus
)

// Status types, as seen by a client.
const (
	StatusUp       = "up"       // upstream connected: the machine is there
	StatusDown     = "down"     // upstream lost: the machine went away
	StatusAttached = "attached" // this subscription is live
	StatusLagged   = "lagged"   // this subscriber fell too far behind
	StatusControl  = "control"  // lease state, pushed by the API layer
)

// StatusMessage is the payload of a text frame.
type StatusMessage struct {
	Type    string `json:"type"`
	Machine string `json:"machine"`
	Message string `json:"message,omitempty"`

	// Holder and Expires describe the write lease, for StatusControl.
	Holder  string     `json:"holder,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
	Write   *bool      `json:"write,omitempty"`

	At time.Time `json:"at"`
}

// Event is one item in a subscriber's stream. Output and status share the
// channel so that their order is preserved -- "the machine went away" must
// arrive after the last bytes the machine printed, not racing them.
type Event struct {
	Kind   EventKind
	Data   []byte
	Status *StatusMessage
}

// Subscriber receives a machine's serial stream.
type Subscriber struct {
	events chan Event
	broker *Broker

	closeOnce sync.Once
	done      chan struct{}

	mu     sync.Mutex
	lagged bool
}

// Events is the stream to forward to a client.
func (s *Subscriber) Events() <-chan Event { return s.events }

// Lagged reports whether this subscriber was dropped for falling behind,
// which a client should surface as "reconnect for fresh scrollback" rather
// than as a clean end of stream.
func (s *Subscriber) Lagged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lagged
}

// Close detaches the subscriber. Safe to call more than once.
//
// Must not be called while holding the broker lock -- it unregisters, which
// takes that lock. The fan-out path uses shutdown instead.
func (s *Subscriber) Close() {
	s.shutdown()
	if s.broker != nil {
		s.broker.detach(s)
	}
}

// shutdown ends the stream without unregistering. Safe under the broker lock,
// which is why the lag path uses it: the broker reaps dead subscribers from
// its own map on the next fan-out.
func (s *Subscriber) shutdown() {
	s.closeOnce.Do(func() { close(s.done) })
}

// dead reports whether the stream has ended.
func (s *Subscriber) dead() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// PushStatus delivers a status event to this subscriber only. The API layer
// uses it for lease changes, which the broker knows nothing about.
func (s *Subscriber) PushStatus(msg StatusMessage) {
	if msg.At.IsZero() {
		msg.At = time.Now()
	}
	m := msg
	s.deliver(Event{Kind: EventStatus, Status: &m})
}

// deliver enqueues without blocking. A subscriber that cannot keep up is
// dropped rather than allowed to stall the fan-out: the upstream read loop
// serves every other subscriber and the transcript, so it must never wait on
// one slow browser.
func (s *Subscriber) deliver(ev Event) {
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.events <- ev:
	default:
		s.mu.Lock()
		already := s.lagged
		s.lagged = true
		s.mu.Unlock()
		if !already {
			// Best effort notice, then drop. Reconnecting gets the client
			// a coherent stream again; dribbling bytes from the middle of
			// a stream would not.
			select {
			case s.events <- Event{Kind: EventStatus, Status: &StatusMessage{
				Type:    StatusLagged,
				Message: "subscriber fell behind; reattach for fresh scrollback",
				At:      time.Now(),
			}}:
			default:
			}
			s.shutdown()
		}
	}
}

// UpstreamState describes the broker's connection to QEMU.
type UpstreamState struct {
	Connected     bool      `json:"connected"`
	Since         time.Time `json:"since,omitzero"`
	LastError     string    `json:"lastError,omitempty"`
	Attempts      int       `json:"attempts"`
	BytesReceived int64     `json:"bytesReceived"`
	Subscribers   int       `json:"subscribers"`
	Transcript    string    `json:"transcript,omitempty"`
}

// Config parameters for one broker.
type Config struct {
	MachineID string
	Title     string
	Network   string // "unix" or "tcp"
	Address   string

	RingBytes       int
	SubscriberQueue int
	BackoffMin      time.Duration
	BackoffMax      time.Duration
	DialTimeout     time.Duration

	Transcripts TranscriptPolicy

	// Dial is injectable so tests can drive a broker without a real QEMU.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	Log *slog.Logger
}

func (c *Config) withDefaults() {
	if c.RingBytes <= 0 {
		c.RingBytes = DefaultRingBytes
	}
	if c.SubscriberQueue <= 0 {
		c.SubscriberQueue = DefaultSubscriberQueue
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = DefaultBackoffMin
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = DefaultBackoffMax
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Dial == nil {
		c.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, network, address)
		}
	}
}

// Broker holds the single upstream connection to one machine's serial socket
// and fans it out to every subscriber.
//
// It starts eagerly, at process start, and keeps reconnecting whether or not
// anybody is watching. That is the point: a VM that reboots unobserved is
// still captured, so the next person to open the machine sees the boot they
// missed (design section 11).
type Broker struct {
	cfg  Config
	ring *Ring
	log  *slog.Logger

	// mu guards everything below, and also serialises the ring via its
	// internal write/snapshot methods. Holding one lock across the ring
	// update and the fan-out is what makes Attach's snapshot coherent.
	mu         sync.Mutex
	subs       map[*Subscriber]struct{}
	conn       net.Conn
	state      UpstreamState
	transcript *Transcript
	closed     bool
}

// NewBroker creates a broker. Call Run to start it.
func NewBroker(cfg Config) *Broker {
	cfg.withDefaults()
	return &Broker{
		cfg:  cfg,
		ring: NewRing(cfg.RingBytes),
		log:  cfg.Log.With("machine", cfg.MachineID),
		subs: make(map[*Subscriber]struct{}),
	}
}

// MachineID identifies the machine this broker serves.
func (b *Broker) MachineID() string { return b.cfg.MachineID }

// Address reports the upstream endpoint, for reconciliation against a
// reloaded inventory.
func (b *Broker) Address() (network, address string) {
	return b.cfg.Network, b.cfg.Address
}

// State returns a snapshot of the upstream connection state.
func (b *Broker) State() UpstreamState {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state
	s.Subscribers = len(b.subs)
	s.BytesReceived = b.ring.Total()
	if b.transcript != nil {
		s.Transcript = b.transcript.Path()
	}
	return s
}

// Attach registers a subscriber and returns it together with the scrollback
// it should be sent first.
//
// Snapshot and registration happen under the same lock as the fan-out, so a
// subscriber can neither miss bytes written between the two nor receive a
// byte twice. Getting this wrong is the classic way to corrupt a console.
func (b *Broker) Attach() (*Subscriber, []byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, nil, errors.New("broker is closed")
	}

	sub := &Subscriber{
		events: make(chan Event, b.cfg.SubscriberQueue),
		broker: b,
		done:   make(chan struct{}),
	}
	scrollback := b.ring.Snapshot()
	b.subs[sub] = struct{}{}

	// Replay current state so a client knows whether the machine is up
	// without waiting for the next transition.
	now := time.Now()
	sub.deliver(Event{Kind: EventStatus, Status: &StatusMessage{
		Type:    StatusAttached,
		Machine: b.cfg.MachineID,
		Message: fmt.Sprintf("%d bytes of scrollback", len(scrollback)),
		At:      now,
	}})
	if b.state.Connected {
		sub.deliver(Event{Kind: EventStatus, Status: &StatusMessage{
			Type: StatusUp, Machine: b.cfg.MachineID, At: b.state.Since,
		}})
	} else {
		msg := "machine is not connected"
		if b.state.LastError != "" {
			msg = b.state.LastError
		}
		sub.deliver(Event{Kind: EventStatus, Status: &StatusMessage{
			Type: StatusDown, Machine: b.cfg.MachineID, Message: msg, At: now,
		}})
	}

	return sub, scrollback, nil
}

func (b *Broker) detach(sub *Subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, sub)
}

// Scrollback returns the buffered output a client would receive on attach.
// The broker lock is the ring's only lock, so this is the way in.
func (b *Broker) Scrollback() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ring.Snapshot()
}

// BufferedBytes reports how many bytes of scrollback are held.
func (b *Broker) BufferedBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ring.Len()
}

// Subscribers reports the current subscriber count.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Write sends bytes to the machine. The caller is responsible for having
// checked the write lease; the broker does not know about leases.
//
// A machine that is switched off returns an error here while still serving
// reads, which is what lets a client sit waiting for a boot.
func (b *Broker) Write(p []byte) error {
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	_, err := conn.Write(p)
	return err
}

// ErrNotConnected is returned by Write when there is no upstream connection.
var ErrNotConnected = errors.New("serial upstream is not connected")

// Run maintains the upstream connection until ctx is cancelled.
func (b *Broker) Run(ctx context.Context) {
	backoff := b.cfg.BackoffMin
	for {
		if ctx.Err() != nil {
			b.shutdown()
			return
		}

		conn, err := b.dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				b.shutdown()
				return
			}
			b.noteDialFailure(err)
			// A switched off machine is the normal case, not an incident,
			// so this is deliberately quiet.
			b.log.Debug("serial dial failed", "error", err, "retry_in", backoff)

			select {
			case <-ctx.Done():
				b.shutdown()
				return
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff, b.cfg.BackoffMax)
			continue
		}

		backoff = b.cfg.BackoffMin
		b.serve(ctx, conn)

		select {
		case <-ctx.Done():
			b.shutdown()
			return
		case <-time.After(b.cfg.BackoffMin):
		}
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

func (b *Broker) dial(ctx context.Context) (net.Conn, error) {
	b.mu.Lock()
	b.state.Attempts++
	b.mu.Unlock()

	dctx, cancel := context.WithTimeout(ctx, b.cfg.DialTimeout)
	defer cancel()
	return b.cfg.Dial(dctx, b.cfg.Network, b.cfg.Address)
}

func (b *Broker) noteDialFailure(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state.LastError = err.Error()
}

// serve runs one upstream connection to completion.
//
// A reconnect is a VM lifecycle boundary, so this is also where the
// transcript rotates: one file is one VM run (design section 11).
func (b *Broker) serve(ctx context.Context, conn net.Conn) {
	now := time.Now()

	tr, err := NewTranscript(b.cfg.MachineID, b.cfg.Title, b.cfg.Transcripts, now)
	if err != nil {
		// Losing the capture is worth logging loudly, but it must not cost
		// the developer their console.
		b.log.Error("transcript create failed, continuing without capture", "error", err)
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		conn.Close()
		tr.Close(now)
		return
	}
	b.conn = conn
	b.transcript = tr
	b.state.Connected = true
	b.state.Since = now
	b.state.LastError = ""
	b.mu.Unlock()

	b.log.Info("serial upstream connected",
		"network", b.cfg.Network, "address", b.cfg.Address, "transcript", tr.Path())
	b.broadcastStatus(StatusMessage{Type: StatusUp, Machine: b.cfg.MachineID, At: now})

	// Prune old captures once the new one exists, so retention counts the
	// active file and never deletes it.
	if b.cfg.Transcripts.Enabled() {
		if err := Prune(b.cfg.Transcripts, b.cfg.MachineID, tr.Path(), now); err != nil {
			b.log.Warn("transcript prune failed", "error", err)
		}
	}

	// Close the connection when the context is cancelled so the read below
	// unblocks promptly rather than at the next byte.
	readDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-readDone:
		}
	}()

	readErr := b.readLoop(conn, tr)
	close(readDone)

	end := time.Now()
	b.mu.Lock()
	b.conn = nil
	b.transcript = nil
	b.state.Connected = false
	b.state.Since = time.Time{}
	if readErr != nil {
		b.state.LastError = readErr.Error()
	}
	b.mu.Unlock()

	conn.Close()
	if err := tr.Close(end); err != nil {
		b.log.Warn("transcript close failed", "error", err)
	}

	b.log.Info("serial upstream lost", "error", readErr)
	b.broadcastStatus(StatusMessage{
		Type: StatusDown, Machine: b.cfg.MachineID,
		Message: errString(readErr), At: end,
	})
}

func errString(err error) string {
	if err == nil {
		return "upstream closed"
	}
	return err.Error()
}

// readLoop copies upstream bytes into the ring, the transcript and every
// subscriber. It holds the broker lock while doing so, which is what makes
// Attach's snapshot coherent; every send inside is non-blocking.
func (b *Broker) readLoop(conn net.Conn, tr *Transcript) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			at := time.Now()

			if werr := tr.Write(chunk, at); werr != nil {
				b.log.Warn("transcript write failed", "error", werr)
			}

			// One copy per chunk, shared by every subscriber: the bytes
			// outlive this iteration of buf, but nobody mutates them.
			data := make([]byte, n)
			copy(data, chunk)

			b.mu.Lock()
			b.ring.Write(chunk)
			for sub := range b.subs {
				if sub.dead() {
					delete(b.subs, sub)
					continue
				}
				sub.deliver(Event{Kind: EventOutput, Data: data})
			}
			b.mu.Unlock()
		}
		if err != nil {
			// Flush so a crash right after the last output still leaves a
			// complete capture on disk.
			tr.Flush()
			return err
		}
	}
}

func (b *Broker) broadcastStatus(msg StatusMessage) {
	if msg.At.IsZero() {
		msg.At = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		if sub.dead() {
			delete(b.subs, sub)
			continue
		}
		m := msg
		sub.deliver(Event{Kind: EventStatus, Status: &m})
	}
}

func (b *Broker) shutdown() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	conn := b.conn
	tr := b.transcript
	subs := make([]*Subscriber, 0, len(b.subs))
	for sub := range b.subs {
		subs = append(subs, sub)
	}
	b.conn = nil
	b.transcript = nil
	b.state.Connected = false
	b.mu.Unlock()

	if conn != nil {
		conn.Close()
	}
	tr.Close(time.Now())
	for _, sub := range subs {
		sub.shutdown()
	}
}
