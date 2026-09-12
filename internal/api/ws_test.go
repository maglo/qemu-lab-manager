package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/config"
	"github.com/maglo/qemu-lab-manager/labview/internal/host"
	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
	"github.com/maglo/qemu-lab-manager/labview/internal/serial"
)

// vm is a stand-in for QEMU: a unix socket for the serial chardev and a TCP
// listener for the VNC port.
type vm struct {
	t          *testing.T
	serialPath string

	mu       sync.Mutex
	conn     net.Conn
	received []byte

	accepted chan struct{}
}

func newVM(t *testing.T) *vm {
	t.Helper()
	dir, err := os.MkdirTemp("", "lvws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	v := &vm{t: t, serialPath: filepath.Join(dir, "s"), accepted: make(chan struct{}, 4)}
	ln, err := net.Listen("unix", v.serialPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			v.mu.Lock()
			v.conn = c
			v.mu.Unlock()
			select {
			case v.accepted <- struct{}{}:
			default:
			}
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						v.mu.Lock()
						v.received = append(v.received, buf[:n]...)
						v.mu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return v
}

func (v *vm) waitConnected(d time.Duration) bool {
	select {
	case <-v.accepted:
		return true
	case <-time.After(d):
		return false
	}
}

func (v *vm) print(s string) {
	v.mu.Lock()
	c := v.conn
	v.mu.Unlock()
	if c == nil {
		v.t.Fatal("VM has no serial connection")
	}
	c.Write([]byte(s))
}

func (v *vm) input() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return string(v.received)
}

// wsHarness serves the API over a real HTTP server so a real websocket client
// can connect.
type wsHarness struct {
	ts      *httptest.Server
	vm      *vm
	leases  *lease.Manager
	brokers *serial.Manager
}

func newWSHarness(t *testing.T) *wsHarness {
	t.Helper()
	v := newVM(t)

	dir := t.TempDir()
	invPath := filepath.Join(dir, "inventory.json")
	doc := fmt.Sprintf(`[{"id":"vm1","name":"vm1","serial":%q,"unit":"qemu-vm1.service"}]`, v.serialPath)
	if err := os.WriteFile(invPath, []byte(doc), 0o640); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	w, err := inventory.NewWatcher(invPath, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.InventoryPath = invPath
	cfg.TranscriptDir = ""
	cfg.LeaseIdle = time.Minute
	cfg.LeaseWarn = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	brokers := serial.NewManager(ctx, serial.ManagerConfig{
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 50 * time.Millisecond,
		Log:        log,
	})
	brokers.Reconcile(w.Current())

	leases := lease.NewManager(lease.Options{IdleTimeout: cfg.LeaseIdle, WarnBefore: cfg.LeaseWarn})

	srv := New(Deps{
		Config: cfg, Inventory: w, Brokers: brokers, Leases: leases,
		Hosts: host.NewFake(), Activity: activity.New(50, log), Log: log,
	})
	ts := httptest.NewServer(srv)

	t.Cleanup(func() { ts.Close(); cancel(); brokers.Close() })

	if !v.waitConnected(3 * time.Second) {
		t.Fatal("broker never connected to the VM")
	}
	return &wsHarness{ts: ts, vm: v, leases: leases, brokers: brokers}
}

func (h *wsHarness) dial(t *testing.T, path, user string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(h.ts.URL, "http") + path
	c, resp, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Forwarded-User": []string{user}},
	})
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("dial %s: %v (%s)", path, err, body)
	}
	c.SetReadLimit(1 << 20)
	t.Cleanup(func() { c.CloseNow() })
	return c
}

// client wraps a websocket with a reader goroutine.
//
// A background reader is not incidental: coder/websocket closes the
// connection when a Read's context is cancelled, because a half-read frame
// cannot be resumed. So a test cannot "read for 200ms and carry on" -- it
// reads continuously, as a browser does, and asserts against what has
// accumulated.
type client struct {
	conn *websocket.Conn

	mu       sync.Mutex
	output   []byte
	statuses []serial.StatusMessage
	readErr  error
}

func newClient(t *testing.T, conn *websocket.Conn) *client {
	t.Helper()
	c := &client{conn: conn}
	go func() {
		for {
			kind, data, err := conn.Read(context.Background())
			if err != nil {
				c.mu.Lock()
				c.readErr = err
				c.mu.Unlock()
				return
			}
			c.mu.Lock()
			switch kind {
			case websocket.MessageBinary:
				c.output = append(c.output, data...)
			case websocket.MessageText:
				var msg serial.StatusMessage
				if uerr := json.Unmarshal(data, &msg); uerr != nil {
					c.readErr = fmt.Errorf("status frame is not JSON: %w (%q)", uerr, data)
					c.mu.Unlock()
					return
				}
				c.statuses = append(c.statuses, msg)
			}
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *client) outputStr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.output)
}

func (c *client) statusList() []serial.StatusMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]serial.StatusMessage(nil), c.statuses...)
}

func (c *client) send(t *testing.T, kind websocket.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, kind, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// waitOutput waits for the accumulated output to contain want.
func (c *client) waitOutput(t *testing.T, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(c.outputStr(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output never contained %q; got %q (statuses %+v)", want, c.outputStr(), c.statusList())
}

// waitStatus waits for a status frame of the given type.
func (c *client) waitStatus(t *testing.T, kind string, d time.Duration) serial.StatusMessage {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, s := range c.statusList() {
			if s.Type == kind {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no status of type %q arrived; got %+v", kind, c.statusList())
	return serial.StatusMessage{}
}

func (c *client) hasStatus(kind string) bool {
	for _, s := range c.statusList() {
		if s.Type == kind {
			return true
		}
	}
	return false
}

// The central framing rule: VM output goes in binary frames and viewer status
// in text frames, so a status message can never be mistaken for something the
// machine printed (design section 11).
func TestSerialWSKeepsStatusOutOfTheByteStream(t *testing.T) {
	h := newWSHarness(t)
	c := newClient(t, h.dial(t, "/ws/serial/vm1", "alice"))

	const line = "[    0.000000] Linux version 6.1\r\n"
	h.vm.print(line)
	c.waitOutput(t, line, 3*time.Second)

	// The attach and up notices must have arrived, and as text frames.
	c.waitStatus(t, serial.StatusAttached, 3*time.Second)
	c.waitStatus(t, serial.StatusUp, 3*time.Second)

	// Nothing status-shaped may appear in the byte stream.
	out := c.outputStr()
	if out != line {
		t.Fatalf("output = %q, want exactly %q", out, line)
	}
	for _, marker := range []string{`"type"`, serial.StatusAttached, serial.StatusUp} {
		if strings.Contains(out, marker) {
			t.Errorf("status leaked into the VM byte stream: %q", out)
		}
	}
}

// Scrollback arrives before live bytes, so a client that attaches late still
// reads the boot it missed (design section 3).
func TestSerialWSSendsScrollbackFirst(t *testing.T) {
	h := newWSHarness(t)

	// Output with nobody attached.
	h.vm.print("boot line one\r\n")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, ok := h.brokers.Get("vm1"); ok && b.BufferedBytes() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	c := newClient(t, h.dial(t, "/ws/serial/vm1", "alice"))
	h.vm.print("live line two\r\n")

	want := "boot line one\r\nlive line two\r\n"
	c.waitOutput(t, want, 3*time.Second)
	if got := c.outputStr(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// Without ?write=1 the attach is read-only, and input is dropped rather than
// reaching the machine (design section 5).
func TestSerialWSReadOnlyByDefault(t *testing.T) {
	h := newWSHarness(t)
	c := newClient(t, h.dial(t, "/ws/serial/vm1", "alice"))
	c.waitStatus(t, serial.StatusAttached, 3*time.Second)

	c.send(t, websocket.MessageBinary, []byte("rm -rf /\n"))

	// The client is told why, rather than left wondering.
	refusal := c.waitStatus(t, serial.StatusControl, 3*time.Second)
	if refusal.Write == nil || *refusal.Write {
		t.Errorf("refusal claims the client may write: %+v", refusal)
	}
	if got := h.vm.input(); got != "" {
		t.Fatalf("a read-only client typed into the machine: %q", got)
	}
}

// ?write=1 requests the lease at attach time, which is what a harness wants.
func TestSerialWSWriteParamTakesLeaseAndDeliversInput(t *testing.T) {
	h := newWSHarness(t)
	c := newClient(t, h.dial(t, "/ws/serial/vm1?write=1", "alice"))
	waitUntil(t, 3*time.Second, func() bool { return h.leases.CanWrite("vm1", "alice") },
		"?write=1 did not take the lease")
	c.send(t, websocket.MessageBinary, []byte("uptime\n"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.vm.input() != "uptime\n" {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.vm.input(); got != "uptime\n" {
		t.Fatalf("machine received %q, want %q", got, "uptime\n")
	}
}

// A second developer may watch but not type, and is told who is driving.
func TestSerialWSSecondClientIsReadOnly(t *testing.T) {
	h := newWSHarness(t)

	// Alice drives; her socket stays open for the rest of the test.
	h.dial(t, "/ws/serial/vm1?write=1", "alice")
	waitUntil(t, 3*time.Second, func() bool { return h.leases.CanWrite("vm1", "alice") },
		"alice did not get the lease")

	bob := newClient(t, h.dial(t, "/ws/serial/vm1?write=1", "bob"))

	// Bob is attached and watching, and knows alice has it.
	control := bob.waitStatus(t, serial.StatusControl, 3*time.Second)
	if control.Holder != "alice" {
		t.Errorf("bob was not told who holds control: %+v", control)
	}
	if h.leases.CanWrite("vm1", "bob") {
		t.Fatal("bob took a held lease")
	}

	// Bob's keystrokes do not reach the machine.
	bob.send(t, websocket.MessageBinary, []byte("sabotage\n"))
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(h.vm.input(), "sabotage") {
		t.Fatalf("a read-only client typed into the machine: %q", h.vm.input())
	}

	// But bob still sees output: read is free.
	h.vm.print("visible\r\n")
	bob.waitOutput(t, "visible", 3*time.Second)
}

// Closing the socket releases the lease, which is what stops a closed laptop
// holding a machine (design section 4).
func TestSerialWSCloseReleasesLease(t *testing.T) {
	h := newWSHarness(t)
	conn := h.dial(t, "/ws/serial/vm1?write=1", "alice")
	newClient(t, conn)
	waitUntil(t, 3*time.Second, func() bool { return h.leases.CanWrite("vm1", "alice") },
		"alice did not get the lease")

	conn.Close(websocket.StatusNormalClosure, "done")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.leases.CanWrite("vm1", "alice") {
		time.Sleep(10 * time.Millisecond)
	}
	if h.leases.CanWrite("vm1", "alice") {
		t.Fatal("lease outlived the socket")
	}
}

// A client's text frames are not input: nothing a client says may enter the
// machine's byte stream except as binary.
func TestSerialWSIgnoresClientTextFrames(t *testing.T) {
	h := newWSHarness(t)
	c := newClient(t, h.dial(t, "/ws/serial/vm1?write=1", "alice"))
	waitUntil(t, 3*time.Second, func() bool { return h.leases.CanWrite("vm1", "alice") },
		"alice did not get the lease")

	c.send(t, websocket.MessageText, []byte(`{"type":"control","message":"injected"}`))
	time.Sleep(300 * time.Millisecond)
	if got := h.vm.input(); got != "" {
		t.Fatalf("a text frame reached the machine: %q", got)
	}
}

// A machine with no serial line in the inventory has no serial endpoint,
// reported as an HTTP error a fetch can read rather than a silent close.
func TestSerialWSRejectsMachineWithoutSerial(t *testing.T) {
	h := newHarness(t) // the JSON harness, which has a console-only machine
	rec := h.do("GET", "/ws/serial/no-unit", "alice", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body)
	}
}

func TestConsoleWSRejectsMachineWithoutConsole(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/ws/console/serial-only", "alice", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body)
	}
}

// A machine that is not listening produces an HTTP error, not an upgrade
// followed by an immediate close, so the browser can say something useful.
func TestConsoleWSUnreachableMachineIsHTTPError(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/ws/console/el9-build", "alice", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body)
	}
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
