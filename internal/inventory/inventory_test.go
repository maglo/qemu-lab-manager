package inventory

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const designExample = `[
  {
    "id": "el9-build",
    "name": "el9-build",
    "host": "kvm01",
    "vnc": "10.20.0.11:5901",
    "serial": "/run/qemu/el9-build-serial.sock",
    "notes": "AlmaLinux 9"
  }
]`

func TestParseDesignExample(t *testing.T) {
	set, err := Parse([]byte(designExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if set.Len() != 1 {
		t.Fatalf("got %d machines, want 1", set.Len())
	}
	m, ok := set.Get("el9-build")
	if !ok {
		t.Fatal("machine not found by id")
	}
	if m.Host != "kvm01" || m.VNC != "10.20.0.11:5901" {
		t.Fatalf("unexpected machine: %+v", m)
	}
	if !m.HasConsole() || !m.HasSerial() {
		t.Fatal("machine should have both a console and a serial line")
	}
	// The design's example carries no unit, so power operations are not
	// available for it: the inventory is the allowlist.
	if m.CanPower() {
		t.Fatal("power operations offered for a machine with no unit")
	}
	if got := m.SerialNetwork(); got != "unix" {
		t.Fatalf("SerialNetwork = %q, want unix", got)
	}
}

// The addresses must never reach the browser (design section 6). Assert on
// the serialised bytes rather than on field names, so a future field carrying
// an address is caught too.
func TestViewNeverLeaksAddresses(t *testing.T) {
	set, err := Parse([]byte(`[
	  {"id":"m","name":"m","host":"kvm01","vnc":"10.20.0.11:5901",
	   "serial":"/run/qemu/m.sock","unit":"qemu-m.service","notes":"n"}
	]`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	blob, err := json.Marshal(set.Views())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(blob)

	for _, secret := range []string{"10.20.0.11", "5901", "/run/qemu/m.sock", "qemu-m.service"} {
		if strings.Contains(got, secret) {
			t.Errorf("view leaked %q to the browser: %s", secret, got)
		}
	}
	// It must still say enough for the UI to render.
	if !strings.Contains(got, `"hasConsole":true`) || !strings.Contains(got, `"hasSerial":true`) {
		t.Errorf("view omits the capability flags the UI needs: %s", got)
	}
	if !strings.Contains(got, `"canPower":true`) {
		t.Errorf("view should report power availability: %s", got)
	}
}

// An id becomes a URL path element and a transcript filename, so traversal
// attempts must be rejected at load rather than escaped at each use.
func TestParseRejectsDangerousIDs(t *testing.T) {
	for _, id := range []string{
		"../etc/passwd",
		"a/b",
		`a\b`,
		"..",
		".hidden",
		"has space",
		"",
		"a;b",
		"a\x00b",
	} {
		doc := `[{"id":` + mustJSON(id) + `,"serial":"/run/a.sock"}]`
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
}

func TestParseAcceptsReasonableIDs(t *testing.T) {
	for _, id := range []string{"el9-build", "kvm01.vm", "a_b", "A1", "9lives"} {
		doc := `[{"id":` + mustJSON(id) + `}]`
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("id %q was rejected: %v", id, err)
		}
	}
}

func TestParseRejectsBadUnitNames(t *testing.T) {
	for _, unit := range []string{
		"qemu-m", // no suffix
		"qemu-m.service; rm -rf /",
		"../../qemu.service",
		"qemu m.service",
	} {
		doc := `[{"id":"m","unit":` + mustJSON(unit) + `}]`
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("unit %q was accepted", unit)
		}
	}
	for _, unit := range []string{"qemu-el9.service", "machine@el9.service", "lab.target", "vm.socket"} {
		doc := `[{"id":"m","unit":` + mustJSON(unit) + `}]`
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("unit %q was rejected: %v", unit, err)
		}
	}
}

func TestParseRejectsDuplicateIDs(t *testing.T) {
	_, err := Parse([]byte(`[{"id":"a"},{"id":"a"}]`))
	if err == nil {
		t.Fatal("duplicate id accepted")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestParseRejectsMalformedAddresses(t *testing.T) {
	if _, err := Parse([]byte(`[{"id":"a","vnc":"10.0.0.1"}]`)); err == nil {
		t.Error("vnc without a port was accepted")
	}
	if _, err := Parse([]byte(`[{"id":"a","serial":"not-a-socket"}]`)); err == nil {
		t.Error("serial that is neither a path nor host:port was accepted")
	}
}

// A producer newer than labview may write fields labview does not model. That
// must not break the load, and the unknown field must still be visible on the
// details tab.
func TestParseKeepsUnknownFieldsVerbatim(t *testing.T) {
	doc := `[{"id":"a","name":"a","tags":["build","el9"],"future":42}]`
	set, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse rejected a forward-compatible entry: %v", err)
	}
	m, _ := set.Get("a")
	if !strings.Contains(string(m.Raw), "tags") || !strings.Contains(string(m.Raw), "future") {
		t.Fatalf("raw entry lost unknown fields: %s", m.Raw)
	}
}

func TestParseRejectsNonArray(t *testing.T) {
	if _, err := Parse([]byte(`{"id":"a"}`)); err == nil {
		t.Fatal("a JSON object was accepted as an inventory")
	}
}

func TestDisplayNameFallsBackToID(t *testing.T) {
	set, _ := Parse([]byte(`[{"id":"a"}]`))
	m, _ := set.Get("a")
	if got := m.DisplayName(); got != "a" {
		t.Fatalf("DisplayName = %q, want %q", got, "a")
	}
}

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestWatcherReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	os.WriteFile(path, []byte(`[{"id":"a"}]`), 0o640)

	w, err := NewWatcher(path, 10*time.Millisecond, discard())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := contextWithCancel()
	defer cancel()
	go w.Run(ctx)

	changed := w.Subscribe()

	time.Sleep(20 * time.Millisecond)
	os.WriteFile(path, []byte(`[{"id":"a"},{"id":"b"}]`), 0o640)

	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("no reload notification")
	}
	if w.Current().Len() != 2 {
		t.Fatalf("reloaded set has %d machines, want 2", w.Current().Len())
	}
}

// A half-written or invalid inventory must not take the console wall down.
func TestWatcherKeepsPreviousSetOnBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	os.WriteFile(path, []byte(`[{"id":"good"}]`), 0o640)

	w, err := NewWatcher(path, 10*time.Millisecond, discard())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := contextWithCancel()
	defer cancel()
	go w.Run(ctx)

	time.Sleep(20 * time.Millisecond)
	os.WriteFile(path, []byte(`[{"id":"good"`), 0o640) // truncated write
	time.Sleep(100 * time.Millisecond)

	if w.Current().Len() != 1 {
		t.Fatalf("bad file changed the live set: %d machines", w.Current().Len())
	}
	if _, ok := w.Current().Get("good"); !ok {
		t.Fatal("previously good machine disappeared")
	}

	// And it recovers once the file is valid again.
	os.WriteFile(path, []byte(`[{"id":"good"},{"id":"better"}]`), 0o640)
	deadline := time.Now().Add(3 * time.Second)
	for w.Current().Len() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if w.Current().Len() != 2 {
		t.Fatal("watcher did not recover after the file was fixed")
	}
}

func TestNewWatcherFailsOnBadInitialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	os.WriteFile(path, []byte(`nonsense`), 0o640)
	if _, err := NewWatcher(path, time.Second, discard()); err == nil {
		t.Fatal("watcher started with an unreadable inventory")
	}
	if _, err := NewWatcher(filepath.Join(dir, "absent.json"), time.Second, discard()); err == nil {
		t.Fatal("watcher started with no inventory file")
	}
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
