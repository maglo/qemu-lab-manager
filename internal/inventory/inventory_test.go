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

const designExample = `name: el9-build
host: kvm01
vnc: 10.20.0.11:5901
serial: /run/qemu/el9-build-serial.sock
notes: AlmaLinux 9
`

func TestParseDesignExample(t *testing.T) {
	m, err := ParseMachine("el9-build", []byte(designExample))
	if err != nil {
		t.Fatalf("ParseMachine: %v", err)
	}
	if m.ID != "el9-build" {
		t.Fatalf("id = %q, want the file name", m.ID)
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
	set := mustLoad(t, map[string]string{
		"m.yaml": `name: m
host: kvm01
vnc: 10.20.0.11:5901
serial: /run/qemu/m.sock
control: /run/qemu/m-qmp.sock
unit: qemu-m.service
notes: n
`,
	})

	blob, err := json.Marshal(set.Views())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(blob)

	for _, secret := range []string{"10.20.0.11", "5901", "/run/qemu/m.sock",
		"/run/qemu/m-qmp.sock", "qemu-m.service"} {
		if strings.Contains(got, secret) {
			t.Errorf("view leaked %q to the browser: %s", secret, got)
		}
	}
	// It must still say enough for the UI to render.
	if !strings.Contains(got, `"hasControl":true`) {
		t.Errorf("view omits the control flag the tile needs: %s", got)
	}
	if !strings.Contains(got, `"hasConsole":true`) || !strings.Contains(got, `"hasSerial":true`) {
		t.Errorf("view omits the capability flags the UI needs: %s", got)
	}
	if !strings.Contains(got, `"canPower":true`) {
		t.Errorf("view should report power availability: %s", got)
	}
}

// An id becomes a URL path element and a transcript filename, so a file whose
// name is not usable as an id must be rejected rather than escaped at each
// use.
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
		if _, err := ParseMachine(id, []byte("serial: /run/a.sock\n")); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
}

func TestParseAcceptsReasonableIDs(t *testing.T) {
	for _, id := range []string{"el9-build", "kvm01.vm", "a_b", "A1", "9lives"} {
		if _, err := ParseMachine(id, nil); err != nil {
			t.Errorf("id %q was rejected: %v", id, err)
		}
	}
}

// The file name is the one place the id comes from. A file that sets one too
// would let the name on disk and the name in the API drift apart.
func TestParseRejectsAnIDInTheFile(t *testing.T) {
	_, err := ParseMachine("a", []byte("id: b\n"))
	if err == nil {
		t.Fatal("a file naming its own machine was accepted")
	}
	if !strings.Contains(err.Error(), "file name") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestParseRejectsBadUnitNames(t *testing.T) {
	for _, unit := range []string{
		"qemu-m", // no suffix
		"qemu-m.service; rm -rf /",
		"../../qemu.service",
		"qemu m.service",
	} {
		doc := "unit: " + mustJSON(unit) + "\n"
		if _, err := ParseMachine("m", []byte(doc)); err == nil {
			t.Errorf("unit %q was accepted", unit)
		}
	}
	for _, unit := range []string{"qemu-el9.service", "machine@el9.service", "lab.target", "vm.socket"} {
		doc := "unit: " + mustJSON(unit) + "\n"
		if _, err := ParseMachine("m", []byte(doc)); err != nil {
			t.Errorf("unit %q was rejected: %v", unit, err)
		}
	}
}

func TestParseRejectsMalformedAddresses(t *testing.T) {
	if _, err := ParseMachine("a", []byte("vnc: 10.0.0.1\n")); err == nil {
		t.Error("vnc without a port was accepted")
	}
	if _, err := ParseMachine("a", []byte("serial: not-a-socket\n")); err == nil {
		t.Error("serial that is neither a path nor host:port was accepted")
	}
	if _, err := ParseMachine("a", []byte("control: not-a-socket\n")); err == nil {
		t.Error("control that is neither a path nor host:port was accepted")
	}
}

// The control socket carries input and screen capture (design section 12). It
// is classified exactly as the serial line is, because either is a dial.
func TestControlChannel(t *testing.T) {
	none, err := ParseMachine("a", []byte("name: a\n"))
	if err != nil {
		t.Fatal(err)
	}
	if none.HasControl() {
		t.Error("a machine with no control field reports a control channel")
	}

	unix, err := ParseMachine("a", []byte("control: /var/lib/qemu/a/qmp.sock\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !unix.HasControl() || unix.ControlNetwork() != "unix" {
		t.Errorf("ControlNetwork = %q, want unix", unix.ControlNetwork())
	}

	tcp, err := ParseMachine("a", []byte("control: kvm01:4444\n"))
	if err != nil {
		t.Fatal(err)
	}
	if tcp.ControlNetwork() != "tcp" {
		t.Errorf("ControlNetwork = %q, want tcp", tcp.ControlNetwork())
	}
}

func TestParseRejectsADocumentThatIsNotAMapping(t *testing.T) {
	if _, err := ParseMachine("a", []byte("- one\n- two\n")); err == nil {
		t.Fatal("a YAML list was accepted as a machine")
	}
}

// A producer newer than labview may write settings labview does not model.
// That must not break the load, and the setting must still be visible on the
// details tab.
func TestParseKeepsUnknownSettingsVerbatim(t *testing.T) {
	doc := `name: a
tags:
  - build
  - el9
future: 42
`
	m, err := ParseMachine("a", []byte(doc))
	if err != nil {
		t.Fatalf("ParseMachine rejected a forward-compatible file: %v", err)
	}
	blob, err := json.Marshal(m.Entry)
	if err != nil {
		t.Fatalf("the entry does not survive JSON: %v", err)
	}
	if !strings.Contains(string(blob), "tags") || !strings.Contains(string(blob), "future") {
		t.Fatalf("the entry lost unknown settings: %s", blob)
	}
}

// The details tab serialises the entry as JSON, so a key JSON cannot carry
// must fail at load rather than when a record is served.
func TestParseRejectsAKeyJSONCannotCarry(t *testing.T) {
	if _, err := ParseMachine("a", []byte("extra:\n  ? [1, 2]\n  : three\n")); err == nil {
		t.Fatal("a mapping with a list for a key was accepted")
	}
}

func TestParseAcceptsAnEmptyFile(t *testing.T) {
	m, err := ParseMachine("a", nil)
	if err != nil {
		t.Fatalf("an empty file was rejected: %v", err)
	}
	if m.ID != "a" || m.HasConsole() || m.HasSerial() || m.CanPower() {
		t.Fatalf("unexpected machine: %+v", m)
	}
}

func TestDisplayNameFallsBackToID(t *testing.T) {
	m, _ := ParseMachine("a", nil)
	if got := m.DisplayName(); got != "a" {
		t.Fatalf("DisplayName = %q, want %q", got, "a")
	}
}

func TestLoadDirReadsOneFilePerMachine(t *testing.T) {
	set := mustLoad(t, map[string]string{
		"b.yaml":      "serial: /run/b.sock\n",
		"a.yaml":      "serial: /run/a.sock\n",
		"c.yml":       "serial: /run/c.sock\n",
		"README.md":   "not a machine\n",
		".draft.yaml": "serial: /run/draft.sock\n",
	})
	if set.Len() != 3 {
		t.Fatalf("got %d machines, want 3", set.Len())
	}
	// File order is id order, and the wall shows the set as it comes.
	var ids []string
	for _, m := range set.Machines() {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Fatalf("machines are in %v, want a,b,c", ids)
	}
}

// Both suffixes name the same machine, so one of the two files must lose.
func TestLoadDirRejectsTwoFilesForOneMachine(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"a.yaml": "serial: /run/a.sock\n",
		"a.yml":  "serial: /run/other.sock\n",
	})
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("two files for one machine were accepted")
	}
	if !strings.Contains(err.Error(), "already comes from") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestLoadDirFailsOnABadFile(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"good.yaml": "serial: /run/good.sock\n",
		"bad.yaml":  "serial: [not, a, socket]\n",
	})
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("a bad file loaded")
	}
	if _, err := LoadDir(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("a directory that does not exist loaded")
	}
}

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// A new file and a deleted file must reach the wall at once, so the watch
// carries them long before the backstop scan would.
func TestWatcherSeesANewAndADeletedFile(t *testing.T) {
	dir := writeDir(t, map[string]string{"a.yaml": "serial: /run/a.sock\n"})

	w, err := NewWatcher(dir, time.Hour, discard())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := contextWithCancel()
	defer cancel()
	go w.Run(ctx)

	changed := w.Subscribe()
	write(t, dir, "b.yaml", "serial: /run/b.sock\n")
	waitForChange(t, changed)
	if w.Current().Len() != 2 {
		t.Fatalf("the new machine is missing: %d machines", w.Current().Len())
	}

	if err := os.Remove(filepath.Join(dir, "b.yaml")); err != nil {
		t.Fatal(err)
	}
	waitForChange(t, changed)
	if _, ok := w.Current().Get("b"); ok {
		t.Fatal("the deleted machine is still live")
	}
}

// A file that goes bad takes down one machine at most, and not even that one:
// the entry that last loaded stays live, so a half-written file does not stop
// a running broker.
func TestWatcherKeepsTheLastGoodEntry(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"good.yaml":  "name: good\nserial: /run/good.sock\n",
		"other.yaml": "name: other\n",
	})

	w, err := NewWatcher(dir, time.Hour, discard())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := contextWithCancel()
	defer cancel()
	go w.Run(ctx)

	changed := w.Subscribe()
	write(t, dir, "good.yaml", "name: good\nserial: [half written")
	waitForChange(t, changed)

	if w.Current().Len() != 2 {
		t.Fatalf("a bad file changed the live set: %d machines", w.Current().Len())
	}
	m, ok := w.Current().Get("good")
	if !ok || m.Serial != "/run/good.sock" {
		t.Fatalf("the last good entry is gone: %+v", m)
	}
	if failed := w.Failed(); len(failed) != 1 || failed[0] != "good.yaml" {
		t.Fatalf("the failed file is not reported by name: %v", failed)
	}

	// And it recovers once the file is written fully.
	write(t, dir, "good.yaml", "name: good\nserial: /run/better.sock\n")
	waitForChange(t, changed)
	if m, _ := w.Current().Get("good"); m.Serial != "/run/better.sock" {
		t.Fatalf("the fixed file did not take: %+v", m)
	}
	if len(w.Failed()) != 0 {
		t.Fatalf("the fixed file is still reported as failed: %v", w.Failed())
	}
}

// The backstop scan carries a change that the watch missed.
func TestWatcherRescansWithoutTheWatch(t *testing.T) {
	dir := writeDir(t, map[string]string{"a.yaml": ""})

	w, err := NewWatcher(dir, 10*time.Millisecond, discard())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	changed := w.Subscribe()

	// Run the backstop alone: the file appears before the watch starts.
	write(t, dir, "b.yaml", "")
	ctx, cancel := contextWithCancel()
	defer cancel()
	go w.Run(ctx)

	waitForChange(t, changed)
	if w.Current().Len() != 2 {
		t.Fatalf("the rescan missed a file: %d machines", w.Current().Len())
	}
}

func TestNewWatcherFailsOnABadDirectory(t *testing.T) {
	dir := writeDir(t, map[string]string{"a.yaml": "unit: nonsense\n"})
	if _, err := NewWatcher(dir, time.Second, discard()); err == nil {
		t.Fatal("the watcher started with an unreadable machine file")
	}
	if _, err := NewWatcher(filepath.Join(dir, "absent"), time.Second, discard()); err == nil {
		t.Fatal("the watcher started with no inventory directory")
	}
}

func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		write(t, dir, name, body)
	}
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T, files map[string]string) *Set {
	t.Helper()
	set, err := LoadDir(writeDir(t, files))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	return set
}

func waitForChange(t *testing.T, changed <-chan struct{}) {
	t.Helper()
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no reload notification")
	}
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
