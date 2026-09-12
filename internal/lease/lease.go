// Package lease implements the write lease: read is free, write is leased.
//
// One machine has one driver at a time regardless of which channel they are
// driving it through, because a scenario is routinely half-serial and half-GUI
// (design section 4). The same lease also gates power operations, so
// restarting a machine somebody else is driving is as impossible as typing
// into it (design section 12).
package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Defaults for the lease lifetime. "A few minutes of no input" from design
// section 4; the warning window exists because section 11 lists "your control
// is about to expire" as a status a client is told about.
const (
	DefaultIdleTimeout = 3 * time.Minute
	DefaultWarnBefore  = 30 * time.Second
	DefaultSweep       = 2 * time.Second
)

// Errors returned by the manager.
var (
	// ErrHeld means somebody else is driving. There is deliberately no way
	// to steal: expiry covers the developer who closed their laptop, and
	// stealing is explicitly deferred (design section 9).
	ErrHeld = errors.New("lease is held by another user")
	// ErrNotHolder means the caller tried to renew or release a lease it
	// does not hold.
	ErrNotHolder = errors.New("caller does not hold the lease")
	// ErrNoHolder means there is no lease to act on.
	ErrNoHolder = errors.New("no lease is held")
)

// Lease describes who is driving a machine.
type Lease struct {
	Machine  string    `json:"machine"`
	Holder   string    `json:"holder"`
	Acquired time.Time `json:"acquired"`
	Expires  time.Time `json:"expires"`
}

// HeldBy reports whether this lease belongs to holder.
func (l Lease) HeldBy(holder string) bool {
	return l.Holder != "" && l.Holder == holder
}

// Event types published by the manager, so that a client attached to a
// machine can be told about control changes as they happen.
const (
	EventGranted  = "granted"
	EventRenewed  = "renewed"
	EventReleased = "released"
	EventExpiring = "expiring"
	EventExpired  = "expired"
)

// Event is a lease transition.
type Event struct {
	Type    string
	Machine string
	Lease   Lease
	At      time.Time
}

type entry struct {
	lease Lease

	// conns counts the live websockets that are holding this lease open. A
	// developer with the console and the serial tab open on one machine has
	// two; closing one must not drop their control. The lease ends when the
	// last one closes -- or on expiry, for a lease taken over the API by a
	// harness that holds no websocket at all.
	conns map[uint64]struct{}

	// tiedToConns is true when the lease was acquired by a websocket, which
	// is what makes "the websocket closed" a release.
	tiedToConns bool

	warned bool
}

// Manager tracks one lease per machine.
type Manager struct {
	idle time.Duration
	warn time.Duration
	now  func() time.Time

	mu      sync.Mutex
	leases  map[string]*entry
	nextID  uint64
	subs    map[uint64]chan Event
	nextSub uint64
}

// Options configures a Manager.
type Options struct {
	IdleTimeout time.Duration
	WarnBefore  time.Duration

	// Now is injectable so tests can drive expiry without sleeping.
	Now func() time.Time
}

// NewManager returns a lease manager.
func NewManager(opts Options) *Manager {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.WarnBefore <= 0 || opts.WarnBefore >= opts.IdleTimeout {
		opts.WarnBefore = min(DefaultWarnBefore, opts.IdleTimeout/2)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Manager{
		idle:   opts.IdleTimeout,
		warn:   opts.WarnBefore,
		now:    opts.Now,
		leases: make(map[string]*entry),
		subs:   make(map[uint64]chan Event),
	}
}

// IdleTimeout reports the configured lease lifetime.
func (m *Manager) IdleTimeout() time.Duration { return m.idle }

// Get returns the current lease for a machine, expiring it first if it is
// due. The bool is false when nobody is driving.
func (m *Manager) Get(machine string) (Lease, bool) {
	m.mu.Lock()
	events := m.expireDue()
	e, ok := m.leases[machine]
	var l Lease
	if ok {
		l = e.lease
	}
	m.mu.Unlock()

	m.publish(events)
	return l, ok
}

// All returns every held lease, keyed by machine, for the machines listing.
func (m *Manager) All() map[string]Lease {
	m.mu.Lock()
	events := m.expireDue()
	out := make(map[string]Lease, len(m.leases))
	for id, e := range m.leases {
		out[id] = e.lease
	}
	m.mu.Unlock()

	m.publish(events)
	return out
}

// Acquire takes the lease for holder.
//
// Acquiring a lease one already holds renews it, so a client need not track
// whether it asked before. Acquiring one somebody else holds fails with
// ErrHeld and the current lease, which is what lets the UI say who is driving
// instead of just refusing.
func (m *Manager) Acquire(machine, holder string) (Lease, error) {
	l, _, err := m.acquire(machine, holder, 0, false)
	return l, err
}

// AcquireConn takes the lease on behalf of a websocket and returns a token to
// pass to ReleaseConn when that socket closes.
func (m *Manager) AcquireConn(machine, holder string) (Lease, uint64, error) {
	return m.acquire(machine, holder, 0, true)
}

func (m *Manager) acquire(machine, holder string, _ uint64, viaConn bool) (Lease, uint64, error) {
	if holder == "" {
		return Lease{}, 0, errors.New("lease holder must be identified")
	}

	m.mu.Lock()
	events := m.expireDue()
	now := m.now()

	e, held := m.leases[machine]
	if held && !e.lease.HeldBy(holder) {
		current := e.lease
		m.mu.Unlock()
		m.publish(events)
		return current, 0, fmt.Errorf("%w: %s", ErrHeld, current.Holder)
	}

	var token uint64
	if !held {
		e = &entry{
			lease: Lease{
				Machine:  machine,
				Holder:   holder,
				Acquired: now,
				Expires:  now.Add(m.idle),
			},
			conns:       make(map[uint64]struct{}),
			tiedToConns: viaConn,
		}
		m.leases[machine] = e
		events = append(events, Event{Type: EventGranted, Machine: machine, Lease: e.lease, At: now})
	} else {
		e.lease.Expires = now.Add(m.idle)
		e.warned = false
		events = append(events, Event{Type: EventRenewed, Machine: machine, Lease: e.lease, At: now})
	}

	if viaConn {
		m.nextID++
		token = m.nextID
		e.conns[token] = struct{}{}
		e.tiedToConns = true
	}

	l := e.lease
	m.mu.Unlock()

	m.publish(events)
	return l, token, nil
}

// Renew extends a lease the caller already holds.
func (m *Manager) Renew(machine, holder string) (Lease, error) {
	m.mu.Lock()
	events := m.expireDue()
	e, ok := m.leases[machine]
	if !ok {
		m.mu.Unlock()
		m.publish(events)
		return Lease{}, ErrNoHolder
	}
	if !e.lease.HeldBy(holder) {
		current := e.lease
		m.mu.Unlock()
		m.publish(events)
		return current, ErrNotHolder
	}

	now := m.now()
	e.lease.Expires = now.Add(m.idle)
	e.warned = false
	l := e.lease
	events = append(events, Event{Type: EventRenewed, Machine: machine, Lease: l, At: now})
	m.mu.Unlock()

	m.publish(events)
	return l, nil
}

// Touch extends the lease because input arrived. It is the mechanism behind
// "expires after a few minutes of no input": every keystroke, on either
// channel, pushes the deadline out.
//
// It reports whether holder may write, so callers can use it as the single
// gate in front of an input path.
func (m *Manager) Touch(machine, holder string) bool {
	m.mu.Lock()
	events := m.expireDue()
	e, ok := m.leases[machine]
	if !ok || !e.lease.HeldBy(holder) {
		m.mu.Unlock()
		m.publish(events)
		return false
	}
	e.lease.Expires = m.now().Add(m.idle)
	e.warned = false
	m.mu.Unlock()

	m.publish(events)
	return true
}

// CanWrite reports whether holder currently holds the machine's lease,
// without extending it. Power operations use this: pressing restart is an
// exercise of control, but it should not silently prolong it.
func (m *Manager) CanWrite(machine, holder string) bool {
	l, ok := m.Get(machine)
	return ok && l.HeldBy(holder)
}

// Release gives up a lease.
func (m *Manager) Release(machine, holder string) error {
	m.mu.Lock()
	events := m.expireDue()
	e, ok := m.leases[machine]
	if !ok {
		m.mu.Unlock()
		m.publish(events)
		return ErrNoHolder
	}
	if !e.lease.HeldBy(holder) {
		m.mu.Unlock()
		m.publish(events)
		return ErrNotHolder
	}
	l := e.lease
	delete(m.leases, machine)
	events = append(events, Event{Type: EventReleased, Machine: machine, Lease: l, At: m.now()})
	m.mu.Unlock()

	m.publish(events)
	return nil
}

// ReleaseConn drops a websocket's hold on a lease. The lease ends when the
// last holding socket closes, so a developer with two tabs open on one
// machine keeps control until both are gone.
func (m *Manager) ReleaseConn(machine, holder string, token uint64) {
	if token == 0 {
		return
	}
	m.mu.Lock()
	events := m.expireDue()
	e, ok := m.leases[machine]
	if !ok || !e.lease.HeldBy(holder) {
		m.mu.Unlock()
		m.publish(events)
		return
	}
	delete(e.conns, token)
	if e.tiedToConns && len(e.conns) == 0 {
		l := e.lease
		delete(m.leases, machine)
		events = append(events, Event{Type: EventReleased, Machine: machine, Lease: l, At: m.now()})
	}
	m.mu.Unlock()

	m.publish(events)
}

// expireDue removes lapsed leases and returns the events to publish. Caller
// holds the lock; publishing happens after it is dropped.
func (m *Manager) expireDue() []Event {
	now := m.now()
	var events []Event
	for id, e := range m.leases {
		if now.Before(e.lease.Expires) {
			continue
		}
		events = append(events, Event{Type: EventExpired, Machine: id, Lease: e.lease, At: now})
		delete(m.leases, id)
	}
	return events
}

// Sweep expires lapsed leases and warns holders whose control is about to
// lapse. Run calls it on a ticker; it is exported so tests can step time.
func (m *Manager) Sweep() {
	m.mu.Lock()
	events := m.expireDue()
	now := m.now()
	for id, e := range m.leases {
		if e.warned || now.Add(m.warn).Before(e.lease.Expires) {
			continue
		}
		e.warned = true
		events = append(events, Event{Type: EventExpiring, Machine: id, Lease: e.lease, At: now})
	}
	m.mu.Unlock()

	m.publish(events)
}

// Subscribe returns a channel of lease events and a function to stop it. The
// API layer forwards these to attached clients as status frames.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	m.mu.Lock()
	m.nextSub++
	id := m.nextSub
	m.subs[id] = ch
	m.mu.Unlock()

	return ch, func() {
		m.mu.Lock()
		if c, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(c)
		}
		m.mu.Unlock()
	}
}

// publish fans events out to subscribers. Never called with the lock held: a
// subscriber is free to call back into the manager.
func (m *Manager) publish(events []Event) {
	if len(events) == 0 {
		return
	}
	m.mu.Lock()
	subs := make([]chan Event, 0, len(m.subs))
	for _, ch := range m.subs {
		subs = append(subs, ch)
	}
	m.mu.Unlock()

	for _, ev := range events {
		for _, ch := range subs {
			select {
			case ch <- ev:
			default: // a subscriber that cannot keep up misses transitions
			}
		}
	}
}

// Run sweeps on a ticker until ctx is cancelled.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultSweep
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Sweep()
		}
	}
}
