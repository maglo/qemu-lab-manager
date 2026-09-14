package qmp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeQEMU answers on a unix socket the way qemu-kvm 10.1 does: a greeting
// first, then one reply per command.
type fakeQEMU struct {
	t   *testing.T
	dir string

	// events makes the server write an asynchronous event line before every
	// reply, which is the case a client must survive.
	events bool
	// png is what screendump writes, so a test can prove labview read the
	// file QEMU wrote rather than inventing one.
	png []byte

	mu        sync.Mutex
	commands  []string
	keys      [][]string
	holds     []int
	filenames []string
}

func (f *fakeQEMU) listen() string {
	f.t.Helper()
	path := filepath.Join(f.dir, "qmp.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return path
}

func (f *fakeQEMU) serve(conn net.Conn) {
	defer conn.Close()

	write := func(v any) bool {
		b, _ := json.Marshal(v)
		_, err := conn.Write(append(b, '\n'))
		return err == nil
	}

	if !write(map[string]any{"QMP": map[string]any{
		"version":      map[string]any{"package": "fake"},
		"capabilities": []string{},
	}}) {
		return
	}

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var req struct {
			Execute   string         `json:"execute"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			write(map[string]any{"error": map[string]string{
				"class": "GenericError", "desc": "not JSON"}})
			continue
		}

		f.mu.Lock()
		f.commands = append(f.commands, req.Execute)
		f.mu.Unlock()

		if f.events {
			write(map[string]any{
				"event":     "RTC_CHANGE",
				"timestamp": map[string]int{"seconds": 1, "microseconds": 2},
				"data":      map[string]int{"offset": 1},
			})
		}

		switch req.Execute {
		case "qmp_capabilities":
			write(map[string]any{"return": map[string]any{}})
		case "send-key":
			f.recordKeys(req.Arguments)
			write(map[string]any{"return": map[string]any{}})
		case "screendump":
			path, _ := req.Arguments["filename"].(string)
			f.mu.Lock()
			f.filenames = append(f.filenames, path)
			f.mu.Unlock()
			if format, _ := req.Arguments["format"].(string); format != "png" {
				write(map[string]any{"error": map[string]string{
					"class": "GenericError", "desc": "expected format png, got " + format}})
				continue
			}
			if err := os.WriteFile(path, f.png, 0o644); err != nil {
				write(map[string]any{"error": map[string]string{
					"class": "GenericError", "desc": err.Error()}})
				continue
			}
			write(map[string]any{"return": map[string]any{}})
		default:
			write(map[string]any{"error": map[string]string{
				"class": "CommandNotFound", "desc": "unknown command " + req.Execute}})
		}
	}
}

func (f *fakeQEMU) recordKeys(arguments map[string]any) {
	var keys []string
	list, _ := arguments["keys"].([]any)
	for _, k := range list {
		entry, _ := k.(map[string]any)
		if entry["type"] != "qcode" {
			continue
		}
		data, _ := entry["data"].(string)
		keys = append(keys, data)
	}
	hold, _ := arguments["hold-time"].(float64)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, keys)
	f.holds = append(f.holds, int(hold))
}

func (f *fakeQEMU) sent() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys
}

func newFake(t *testing.T, events bool) (*fakeQEMU, Target) {
	t.Helper()
	f := &fakeQEMU{t: t, dir: t.TempDir(), events: events, png: []byte("\x89PNG\r\n\x1a\nfake frame")}
	return f, Target{ID: "vm1", Network: "unix", Address: f.listen()}
}

func managerFor(t *testing.T, captureDir string) *Manager {
	t.Helper()
	return NewManager(Options{CaptureDir: captureDir, Timeout: 5 * time.Second})
}

// A chord is one array, pressed together and released together.
func TestSendKeyPressesOneChord(t *testing.T) {
	f, target := newFake(t, false)
	m := managerFor(t, t.TempDir())

	if err := m.SendKeys(context.Background(), target, []string{"ctrl", "alt", "f3"}, 100); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	sent := f.sent()
	if len(sent) != 1 || strings.Join(sent[0], "+") != "ctrl+alt+f3" {
		t.Fatalf("QEMU received %v, want one chord of ctrl alt f3", sent)
	}
	f.mu.Lock()
	hold := f.holds[0]
	commands := strings.Join(f.commands, ",")
	f.mu.Unlock()
	if hold != 100 {
		t.Errorf("hold-time = %d, want 100", hold)
	}
	// QEMU runs no other command until the greeting is answered.
	if !strings.HasPrefix(commands, "qmp_capabilities,") {
		t.Errorf("commands = %s, want qmp_capabilities first", commands)
	}
}

// An event line arrives whenever QEMU feels like it. A client that takes the
// next line for its reply reads the wrong one for every command after it, and
// the symptom looks like screendump returning before the file exists.
func TestEventBetweenCommandAndReplyIsSkipped(t *testing.T) {
	f, target := newFake(t, true)
	dir := t.TempDir()
	m := managerFor(t, dir)

	if err := m.SendKeys(context.Background(), target, []string{"ret"}, 0); err != nil {
		t.Fatalf("SendKeys with an event in the way: %v", err)
	}
	png, err := m.Screenshot(context.Background(), target)
	if err != nil {
		t.Fatalf("Screenshot with an event in the way: %v", err)
	}
	if string(png) != string(f.png) {
		t.Errorf("got %q, want the frame QEMU wrote", png)
	}
}

// The file is QEMU's, so labview reads it and removes it again.
func TestScreenshotReadsAndRemovesTheFile(t *testing.T) {
	f, target := newFake(t, false)
	dir := t.TempDir()
	m := managerFor(t, dir)

	png, err := m.Screenshot(context.Background(), target)
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if string(png) != string(f.png) {
		t.Errorf("got %q, want the frame QEMU wrote", png)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the capture directory still holds %d file(s); a frame of a screen must not stay on disk", len(left))
	}
}

// QEMU resolves the filename itself, in its own filesystem namespace, so a
// relative path would land wherever QEMU happens to run.
func TestScreenshotPathIsAbsolute(t *testing.T) {
	f, target := newFake(t, false)
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("captures", 0o750); err != nil {
		t.Fatal(err)
	}
	m := managerFor(t, "captures")

	if _, err := m.Screenshot(context.Background(), target); err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.filenames) != 1 || !filepath.IsAbs(f.filenames[0]) {
		t.Errorf("QEMU was asked to write %v, want one absolute path", f.filenames)
	}
}

func TestScreenshotWithoutACaptureDirectory(t *testing.T) {
	_, target := newFake(t, false)
	m := managerFor(t, "")
	if m.CaptureEnabled() {
		t.Error("capture is enabled with no directory")
	}
	if _, err := m.Screenshot(context.Background(), target); err != ErrNoCaptureDir {
		t.Errorf("error = %v, want ErrNoCaptureDir", err)
	}
}

// QEMU's own refusal names the command and the parameter, never the socket,
// so the API is allowed to show it.
func TestQEMURefusalIsACommandError(t *testing.T) {
	_, target := newFake(t, false)
	m := managerFor(t, t.TempDir())

	err := m.with(context.Background(), target, func(ctx context.Context, c *Client) error {
		_, err := c.Execute(ctx, "human-monitor-command", nil)
		return err
	})
	var refused *CommandError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want a CommandError", err)
	}
	if refused.Class != "CommandNotFound" {
		t.Errorf("class = %q", refused.Class)
	}
}

func TestKeysAreValidated(t *testing.T) {
	cases := map[string][]string{
		"no keys":        {},
		"empty name":     {""},
		"upper case":     {"Ctrl"},
		"a shell string": {"ctrl; reboot"},
		"too many":       {"a", "b", "c", "d", "e", "f", "g", "h", "i"},
	}
	for name, keys := range cases {
		if err := ValidateKeys(keys); err == nil {
			t.Errorf("%s: accepted %v", name, keys)
		}
	}
	if err := ValidateKeys([]string{"ctrl", "alt", "f3", "kp_enter"}); err != nil {
		t.Errorf("rejected a valid chord: %v", err)
	}
}

// One socket takes one client at a time, so the manager must not let two
// callers onto a machine at once.
//
// The count is of open connections rather than of sessions the fake saw: the
// manager closes a connection before it releases the machine, so an overlap
// here is the manager's and not the fake's shutdown lagging behind.
func TestExchangesOfOneMachineAreSerialised(t *testing.T) {
	f, target := newFake(t, false)

	var mu sync.Mutex
	open, overlap := 0, false
	m := NewManager(Options{
		CaptureDir: t.TempDir(),
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			open++
			if open > 1 {
				overlap = true
			}
			mu.Unlock()
			return &countedConn{Conn: conn, closed: func() {
				mu.Lock()
				open--
				mu.Unlock()
			}}, nil
		},
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.SendKeys(context.Background(), target, []string{"ret"}, 0); err != nil {
				t.Errorf("SendKeys: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(f.sent()); got != 8 {
		t.Errorf("QEMU received %d chords, want 8", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if overlap {
		t.Error("two clients were on the control socket at once")
	}
}

// countedConn reports when it closes, once, however often Close is called.
type countedConn struct {
	net.Conn
	once   sync.Once
	closed func()
}

func (c *countedConn) Close() error {
	c.once.Do(c.closed)
	return c.Conn.Close()
}
