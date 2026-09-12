package serial

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeQEMU stands in for a -chardev socket: a unix listener that accepts one
// connection at a time and lets the test push bytes at it.
type fakeQEMU struct {
	t    *testing.T
	path string
	ln   net.Listener

	mu    sync.Mutex
	conn  net.Conn
	input bytes.Buffer // what the broker wrote towards the "VM"

	accepted chan struct{}
}

func newFakeQEMU(t *testing.T) *fakeQEMU {
	t.Helper()
	// Keep the path short: a unix socket path is capped near 108 bytes and
	// t.TempDir() under a long test name can overflow it.
	dir, err := os.MkdirTemp("", "lv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	q := &fakeQEMU{t: t, path: filepath.Join(dir, "s"), accepted: make(chan struct{}, 8)}
	ln, err := net.Listen("unix", q.path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	q.ln = ln
	go q.acceptLoop()
	t.Cleanup(func() { ln.Close() })
	return q
}

func (q *fakeQEMU) acceptLoop() {
	for {
		c, err := q.ln.Accept()
		if err != nil {
			return
		}
		q.mu.Lock()
		q.conn = c
		q.mu.Unlock()
		select {
		case q.accepted <- struct{}{}:
		default:
		}
		go func(c net.Conn) {
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					q.mu.Lock()
					q.input.Write(buf[:n])
					q.mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}(c)
	}
}

func (q *fakeQEMU) waitAccepted(d time.Duration) bool {
	select {
	case <-q.accepted:
		return true
	case <-time.After(d):
		return false
	}
}

func (q *fakeQEMU) send(s string) {
	q.mu.Lock()
	c := q.conn
	q.mu.Unlock()
	if c == nil {
		q.t.Fatal("fake QEMU has no connection")
	}
	if _, err := c.Write([]byte(s)); err != nil {
		q.t.Fatalf("fake QEMU write: %v", err)
	}
}

// drop closes the current connection, simulating the VM going away.
func (q *fakeQEMU) drop() {
	q.mu.Lock()
	c := q.conn
	q.conn = nil
	q.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func (q *fakeQEMU) received() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.input.String()
}

// collect reads events until it has seen wantOutput bytes of output or times
// out, returning output bytes and status types separately -- which is the
// property that matters: they must never be mixed into one stream.
func collect(t *testing.T, sub *Subscriber, wantOutput int, d time.Duration) (string, []string) {
	t.Helper()
	var out bytes.Buffer
	var statuses []string
	deadline := time.After(d)
	for out.Len() < wantOutput {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return out.String(), statuses
			}
			switch ev.Kind {
			case EventOutput:
				out.Write(ev.Data)
			case EventStatus:
				if ev.Status == nil {
					t.Fatal("status event with nil payload")
				}
				if ev.Data != nil {
					t.Fatal("status event carried output bytes")
				}
				statuses = append(statuses, ev.Status.Type)
			}
		case <-deadline:
			return out.String(), statuses
		}
	}
	return out.String(), statuses
}

// collector reads a subscriber continuously in the background, the way a
// websocket writer does. Letting a queue fill while the machine is talking is
// what the lag path is for, so tests that are not about lag must drain.
type collector struct {
	mu       sync.Mutex
	out      bytes.Buffer
	statuses []string
	done     chan struct{}
}

func collectAsync(sub *Subscriber) *collector {
	c := &collector{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for ev := range sub.Events() {
			c.mu.Lock()
			switch ev.Kind {
			case EventOutput:
				c.out.Write(ev.Data)
			case EventStatus:
				c.statuses = append(c.statuses, ev.Status.Type)
			}
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *collector) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

func (c *collector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Len()
}

func (c *collector) statusTypes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.statuses...)
}

func testConfig(q *fakeQEMU) Config {
	return Config{
		MachineID:  "el9-build",
		Network:    "unix",
		Address:    q.path,
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
		Log:        discardLogger(),
	}
}

func TestBrokerFansOutLiveBytes(t *testing.T) {
	q := newFakeQEMU(t)
	b := NewBroker(testConfig(q))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	if !q.waitAccepted(2 * time.Second) {
		t.Fatal("broker never connected")
	}

	// Two subscribers, both read-only; both must see everything.
	s1, _, err := b.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer s1.Close()
	s2, _, err := b.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer s2.Close()

	q.send("hello serial")

	if got, _ := collect(t, s1, 12, 2*time.Second); got != "hello serial" {
		t.Fatalf("s1 got %q", got)
	}
	if got, _ := collect(t, s2, 12, 2*time.Second); got != "hello serial" {
		t.Fatalf("s2 got %q", got)
	}
}

// Scrollback is the reason the broker exists: a client attaching after the
// fact must still see what it missed.
func TestBrokerReplaysScrollbackOnAttach(t *testing.T) {
	q := newFakeQEMU(t)
	b := NewBroker(testConfig(q))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	if !q.waitAccepted(2 * time.Second) {
		t.Fatal("broker never connected")
	}
	q.send("[    0.000000] booting\r\n")

	// Wait for the broker to have buffered it, with nobody attached.
	deadline := time.Now().Add(2 * time.Second)
	for b.BufferedBytes() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	sub, scrollback, err := b.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer sub.Close()

	if string(scrollback) != "[    0.000000] booting\r\n" {
		t.Fatalf("scrollback = %q", scrollback)
	}

	// And live bytes continue from there, with no repeat of the scrollback.
	q.send("login: ")
	got, _ := collect(t, sub, 7, 2*time.Second)
	if got != "login: " {
		t.Fatalf("live output = %q, want %q", got, "login: ")
	}
}

// The no-gap-no-duplicate property under concurrent writes. Attaching while
// bytes are flowing is the classic way to corrupt a console, so hammer it.
func TestBrokerAttachIsAtomicUnderLoad(t *testing.T) {
	q := newFakeQEMU(t)
	b := NewBroker(testConfig(q))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	if !q.waitAccepted(2 * time.Second) {
		t.Fatal("broker never connected")
	}

	// A stream of distinguishable, fixed width records.
	const records = 300
	const recLen = 6
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < records; i++ {
			q.send(fmt.Sprintf("%05d\n", i))
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(20 * time.Millisecond) // let some bytes accumulate
	sub, scrollback, err := b.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer sub.Close()
	c := collectAsync(sub)
	<-done

	// Wait until the stream is contiguous through the final record.
	waitFor(t, 5*time.Second, func() bool {
		return len(scrollback)+c.len() >= records*recLen
	})
	joined := string(scrollback) + c.output()

	if sub.Lagged() {
		t.Fatal("subscriber lagged; this test is about the attach seam, not lag")
	}

	// Every record from the first one present must appear exactly once, in
	// order, with no duplication at the scrollback/live seam.
	var first int
	if len(joined) >= recLen {
		fmt.Sscanf(joined[:5], "%05d", &first)
	}
	var want bytes.Buffer
	for i := first; i < records; i++ {
		fmt.Fprintf(&want, "%05d\n", i)
	}
	if joined != want.String() {
		t.Fatalf("stream corrupted at the scrollback seam\n got len=%d\nwant len=%d\ngot tail=%q\nwant tail=%q",
			len(joined), want.Len(), tail(joined, 60), tail(want.String(), 60))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// A subscriber that never reads must not stall the fan-out for anybody else.
// Before the fix this deadlocked: the lag path re-entered the broker lock
// that the fan-out already held.
func TestBrokerSlowSubscriberDoesNotStallOthers(t *testing.T) {
	q := newFakeQEMU(t)
	cfg := testConfig(q)
	cfg.SubscriberQueue = 4 // tiny, so the one that does not read fills up
	b := NewBroker(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	if !q.waitAccepted(2 * time.Second) {
		t.Fatal("broker never connected")
	}

	slow, _, _ := b.Attach() // attached and deliberately never read
	fast, _, _ := b.Attach()
	defer fast.Close()
	c := collectAsync(fast) // drains continuously, like a live websocket

	const want = "0123456789abcdefghij"
	for _, ch := range want {
		q.send(string(ch))
		time.Sleep(2 * time.Millisecond)
	}

	waitFor(t, 3*time.Second, func() bool { return c.len() >= len(want) })
	if got := c.output(); got != want {
		t.Fatalf("fast subscriber got %q, want %q", got, want)
	}
	if !slow.Lagged() {
		t.Fatal("slow subscriber should have been marked lagged")
	}
	if !slow.dead() {
		t.Fatal("slow subscriber should have been dropped")
	}
	if fast.Lagged() {
		t.Fatal("fast subscriber was penalised for the slow one")
	}
}

// Attaching to a machine that is not running yet is an explicit requirement:
// a developer who wants to watch a boot has to connect before it happens
// (design section 11).
func TestBrokerAttachBeforeMachineExists(t *testing.T) {
	dir, err := os.MkdirTemp("", "lv")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")

	cfg := Config{
		MachineID:  "late",
		Network:    "unix",
		Address:    sock,
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
		Log:        discardLogger(),
	}
	b := NewBroker(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	// Attach while there is nothing to attach to.
	sub, scrollback, err := b.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer sub.Close()
	if len(scrollback) != 0 {
		t.Fatalf("scrollback = %q, want empty", scrollback)
	}
	if st := b.State(); st.Connected {
		t.Fatal("broker reports connected with no socket present")
	}

	// Writing to a machine that is not there fails, but the subscription
	// stays alive.
	if err := b.Write([]byte("x")); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Write error = %v, want ErrNotConnected", err)
	}

	// Now the VM starts.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("[    0.000000] Linux version\r\n"))
	}()

	got, statuses := collect(t, sub, 29, 5*time.Second)
	if got != "[    0.000000] Linux version\r\n" {
		t.Fatalf("output = %q", got)
	}
	// The client must have been told the machine came up, as a status
	// message and not as VM output.
	if !containsStatus(statuses, StatusUp) {
		t.Fatalf("statuses = %v, want one of type %q", statuses, StatusUp)
	}
}

func containsStatus(got []string, want string) bool {
	for _, s := range got {
		if s == want {
			return true
		}
	}
	return false
}

// A reconnect is a VM lifecycle boundary, so it rotates the transcript: one
// file is one VM run (design section 11).
func TestBrokerRotatesTranscriptOnReconnect(t *testing.T) {
	q := newFakeQEMU(t)
	castDir := t.TempDir()

	cfg := testConfig(q)
	cfg.Transcripts = TranscriptPolicy{Dir: castDir}
	b := NewBroker(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	if !q.waitAccepted(2 * time.Second) {
		t.Fatal("broker never connected")
	}
	q.send("first run\r\n")
	waitFor(t, 2*time.Second, func() bool { return b.BufferedBytes() > 0 })

	first := b.State().Transcript
	if first == "" {
		t.Fatal("no transcript for first run")
	}

	q.drop()
	if !q.waitAccepted(3 * time.Second) {
		t.Fatal("broker did not reconnect")
	}
	q.send("second run\r\n")
	waitFor(t, 2*time.Second, func() bool {
		s := b.State()
		return s.Connected && s.Transcript != "" && s.Transcript != first
	})

	second := b.State().Transcript
	if second == first {
		t.Fatalf("transcript did not rotate on reconnect (still %q)", first)
	}

	recs, err := ListRecordings(castDir, "el9-build")
	if err != nil {
		t.Fatalf("ListRecordings: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d captures, want 2 (one per run): %+v", len(recs), recs)
	}

	// Scrollback deliberately survives the reboot, so the panic and the
	// boot that followed it read as one stream.
	if sb := b.Scrollback(); !bytes.Contains(sb, []byte("first run")) {
		t.Fatalf("scrollback lost the previous run: %q", sb)
	}
}

func TestBrokerWriteReachesMachine(t *testing.T) {
	q := newFakeQEMU(t)
	b := NewBroker(testConfig(q))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	if !q.waitAccepted(2 * time.Second) {
		t.Fatal("broker never connected")
	}

	if err := b.Write([]byte("root\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return q.received() == "root\n" })
	if got := q.received(); got != "root\n" {
		t.Fatalf("machine received %q, want %q", got, "root\n")
	}
}

func TestBrokerReportsDownAfterUpstreamLoss(t *testing.T) {
	q := newFakeQEMU(t)
	b := NewBroker(testConfig(q))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	if !q.waitAccepted(2 * time.Second) {
		t.Fatal("broker never connected")
	}

	sub, _, _ := b.Attach()
	defer sub.Close()
	drainStatuses(sub, 200*time.Millisecond)

	q.drop()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("stream closed before a down status arrived")
			}
			if ev.Kind == EventStatus && ev.Status.Type == StatusDown {
				return
			}
		case <-deadline:
			t.Fatal("no down status after upstream loss")
		}
	}
}

func TestBrokerShutdownClosesSubscribers(t *testing.T) {
	q := newFakeQEMU(t)
	b := NewBroker(testConfig(q))
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	if !q.waitAccepted(2 * time.Second) {
		t.Fatal("broker never connected")
	}

	sub, _, _ := b.Attach()
	cancel()

	waitFor(t, 3*time.Second, func() bool { return sub.dead() })
	if !sub.dead() {
		t.Fatal("subscriber outlived the broker")
	}
	if _, _, err := b.Attach(); err == nil {
		t.Fatal("Attach succeeded on a closed broker")
	}
}

func drainStatuses(sub *Subscriber, d time.Duration) {
	deadline := time.After(d)
	for {
		select {
		case <-sub.Events():
		case <-deadline:
			return
		}
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func discardLogger() *slogLogger { return newDiscardLogger() }

var _ io.Writer = io.Discard
