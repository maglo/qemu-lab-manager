package lease

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// clock lets tests step time rather than sleep through a lease lifetime.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
}
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestManager(c *clock) *Manager {
	return NewManager(Options{
		IdleTimeout: time.Minute,
		WarnBefore:  10 * time.Second,
		Now:         c.now,
	})
}

func TestAcquireGrantsWhenFree(t *testing.T) {
	m := newTestManager(newClock())
	l, err := m.Acquire("vm1", "alice")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if l.Holder != "alice" {
		t.Fatalf("holder = %q", l.Holder)
	}
	if !m.CanWrite("vm1", "alice") {
		t.Fatal("holder cannot write")
	}
	// Everyone else may watch but not type.
	if m.CanWrite("vm1", "bob") {
		t.Fatal("non-holder can write")
	}
}

// If held, the developer sees who holds it (design section 4), so the error
// must carry the current lease rather than just refusing.
func TestAcquireDeniedButNamesTheHolder(t *testing.T) {
	m := newTestManager(newClock())
	m.Acquire("vm1", "alice")

	current, err := m.Acquire("vm1", "bob")
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("error = %v, want ErrHeld", err)
	}
	if current.Holder != "alice" {
		t.Fatalf("denied acquire did not report the holder: %+v", current)
	}
	// No stealing in v1 (design section 9): alice still has it.
	if !m.CanWrite("vm1", "alice") {
		t.Fatal("alice lost the lease to a denied request")
	}
}

func TestAcquireByHolderRenews(t *testing.T) {
	c := newClock()
	m := newTestManager(c)
	first, _ := m.Acquire("vm1", "alice")

	c.advance(30 * time.Second)
	second, err := m.Acquire("vm1", "alice")
	if err != nil {
		t.Fatalf("re-acquire by holder failed: %v", err)
	}
	if !second.Expires.After(first.Expires) {
		t.Fatal("re-acquiring did not extend the lease")
	}
	if !second.Acquired.Equal(first.Acquired) {
		t.Fatal("re-acquiring restarted the lease instead of renewing it")
	}
}

// "A lease expires after a few minutes of no input" (design section 4).
func TestLeaseExpiresWhenIdle(t *testing.T) {
	c := newClock()
	m := newTestManager(c)
	m.Acquire("vm1", "alice")

	c.advance(59 * time.Second)
	if !m.CanWrite("vm1", "alice") {
		t.Fatal("lease expired early")
	}

	c.advance(2 * time.Second)
	if m.CanWrite("vm1", "alice") {
		t.Fatal("lease outlived its idle timeout")
	}
	// And the machine is free again, which is what stops a closed laptop
	// blocking everyone.
	if _, err := m.Acquire("vm1", "bob"); err != nil {
		t.Fatalf("machine still locked after expiry: %v", err)
	}
}

// Input pushes the deadline out, on either channel.
func TestTouchKeepsLeaseAlive(t *testing.T) {
	c := newClock()
	m := newTestManager(c)
	m.Acquire("vm1", "alice")

	for i := 0; i < 5; i++ {
		c.advance(50 * time.Second)
		if !m.Touch("vm1", "alice") {
			t.Fatalf("Touch %d refused while typing", i)
		}
	}
	if !m.CanWrite("vm1", "alice") {
		t.Fatal("continuous input did not keep the lease")
	}

	// Touch is also the write gate: it must refuse a non-holder.
	if m.Touch("vm1", "bob") {
		t.Fatal("Touch let a non-holder write")
	}
}

// The lease covers both channels together: one driver per machine regardless
// of which channel they drive it through (design section 4).
func TestOneLeaseCoversBothChannels(t *testing.T) {
	m := newTestManager(newClock())

	// Alice takes control from the console tab.
	_, consoleTok, err := m.AcquireConn("vm1", "alice")
	if err != nil {
		t.Fatalf("console acquire: %v", err)
	}
	// Her serial tab on the same machine shares the lease.
	_, serialTok, err := m.AcquireConn("vm1", "alice")
	if err != nil {
		t.Fatalf("serial acquire by the same holder failed: %v", err)
	}
	// Bob gets nothing on either channel.
	if _, _, err := m.AcquireConn("vm1", "bob"); !errors.Is(err, ErrHeld) {
		t.Fatalf("bob acquired a held machine: %v", err)
	}

	// Closing one tab must not drop her control.
	m.ReleaseConn("vm1", "alice", consoleTok)
	if !m.CanWrite("vm1", "alice") {
		t.Fatal("closing one of two sockets dropped the lease")
	}

	// Closing the last one does.
	m.ReleaseConn("vm1", "alice", serialTok)
	if m.CanWrite("vm1", "alice") {
		t.Fatal("lease survived the last socket closing")
	}
}

// A harness takes the lease over the API and holds no websocket at all, so
// that lease must live until release or expiry.
func TestAPIAcquiredLeaseSurvivesWithNoSockets(t *testing.T) {
	c := newClock()
	m := newTestManager(c)
	if _, err := m.Acquire("vm1", "harness"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// A stray ReleaseConn with no token must not drop it.
	m.ReleaseConn("vm1", "harness", 0)
	if !m.CanWrite("vm1", "harness") {
		t.Fatal("API-acquired lease was dropped with no socket involved")
	}

	c.advance(2 * time.Minute)
	if m.CanWrite("vm1", "harness") {
		t.Fatal("API-acquired lease never expired")
	}
}

func TestReleaseRequiresHolder(t *testing.T) {
	m := newTestManager(newClock())
	m.Acquire("vm1", "alice")

	if err := m.Release("vm1", "bob"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("bob released alice's lease: %v", err)
	}
	if !m.CanWrite("vm1", "alice") {
		t.Fatal("alice lost her lease to someone else's release")
	}
	if err := m.Release("vm1", "alice"); err != nil {
		t.Fatalf("Release by holder: %v", err)
	}
	if err := m.Release("vm1", "alice"); !errors.Is(err, ErrNoHolder) {
		t.Fatalf("second release: %v", err)
	}
}

func TestLeasesAreIndependentPerMachine(t *testing.T) {
	m := newTestManager(newClock())
	m.Acquire("vm1", "alice")
	if _, err := m.Acquire("vm2", "bob"); err != nil {
		t.Fatalf("bob could not take a different machine: %v", err)
	}
	all := m.All()
	if all["vm1"].Holder != "alice" || all["vm2"].Holder != "bob" {
		t.Fatalf("leases got crossed: %+v", all)
	}
}

// "Your control is about to expire" is a status a client is told about
// (design section 11), so the manager must publish it before the fact.
func TestSweepWarnsBeforeExpiryThenExpires(t *testing.T) {
	c := newClock()
	m := newTestManager(c)
	events, stop := m.Subscribe()
	defer stop()

	m.Acquire("vm1", "alice")
	drain(events)

	// Inside the warning window but not yet lapsed.
	c.advance(55 * time.Second)
	m.Sweep()
	if got := nextType(t, events); got != EventExpiring {
		t.Fatalf("event = %q, want %q", got, EventExpiring)
	}
	if !m.CanWrite("vm1", "alice") {
		t.Fatal("warning window revoked control early")
	}

	// Warned once, not on every sweep.
	m.Sweep()
	if got := tryNext(events); got != "" {
		t.Fatalf("repeated warning: %q", got)
	}

	c.advance(10 * time.Second)
	m.Sweep()
	if got := nextType(t, events); got != EventExpired {
		t.Fatalf("event = %q, want %q", got, EventExpired)
	}
	if m.CanWrite("vm1", "alice") {
		t.Fatal("lease survived expiry")
	}
}

// Typing after a warning must clear it, so the next idle period warns again.
func TestTouchClearsExpiryWarning(t *testing.T) {
	c := newClock()
	m := newTestManager(c)
	events, stop := m.Subscribe()
	defer stop()

	m.Acquire("vm1", "alice")
	c.advance(55 * time.Second)
	m.Sweep()
	drain(events)

	m.Touch("vm1", "alice") // alice comes back and types
	drain(events)

	c.advance(55 * time.Second)
	m.Sweep()
	if got := nextType(t, events); got != EventExpiring {
		t.Fatalf("second idle period did not warn again: %q", got)
	}
}

func TestSubscribePublishesTransitions(t *testing.T) {
	m := newTestManager(newClock())
	events, stop := m.Subscribe()
	defer stop()

	m.Acquire("vm1", "alice")
	if got := nextType(t, events); got != EventGranted {
		t.Fatalf("event = %q, want %q", got, EventGranted)
	}
	m.Release("vm1", "alice")
	if got := nextType(t, events); got != EventReleased {
		t.Fatalf("event = %q, want %q", got, EventReleased)
	}
}

func TestAcquireRequiresIdentity(t *testing.T) {
	m := newTestManager(newClock())
	if _, err := m.Acquire("vm1", ""); err == nil {
		t.Fatal("an unidentified caller took a lease")
	}
}

func TestConcurrentAcquireYieldsOneWinner(t *testing.T) {
	m := newTestManager(newClock())
	const contenders = 32

	var wg sync.WaitGroup
	won := make([]bool, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := m.Acquire("vm1", holderName(i)); err == nil {
				won[i] = true
			}
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, w := range won {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d contenders won the lease, want exactly 1", winners)
	}
}

func holderName(i int) string { return string(rune('a'+i%26)) + string(rune('0'+i/26)) }

func drain(ch <-chan Event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func nextType(t *testing.T, ch <-chan Event) string {
	t.Helper()
	select {
	case ev := <-ch:
		return ev.Type
	case <-time.After(time.Second):
		t.Fatal("no event published")
		return ""
	}
}

func tryNext(ch <-chan Event) string {
	select {
	case ev := <-ch:
		return ev.Type
	case <-time.After(50 * time.Millisecond):
		return ""
	}
}
