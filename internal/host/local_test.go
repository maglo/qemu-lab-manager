package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

func machine(t *testing.T, id, doc string) inventory.Machine {
	t.Helper()
	m, err := inventory.ParseMachine(id, []byte(doc))
	if err != nil {
		t.Fatalf("inventory.ParseMachine: %v", err)
	}
	return m
}

// The command line comes from /proc as NUL separated argv, which is what
// makes it the invocation as invoked rather than a reconstruction.
func TestProcessArgsReadsNULSeparatedArgv(t *testing.T) {
	proc := t.TempDir()
	pid := 4242
	if err := os.MkdirAll(filepath.Join(proc, fmt.Sprint(pid)), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := "/usr/libexec/qemu-kvm\x00-m\x004096\x00-name\x00guest=el9\x00"
	os.WriteFile(filepath.Join(proc, fmt.Sprint(pid), "cmdline"), []byte(argv), 0o644)

	l := NewLocal(LocalOptions{ProcRoot: proc, Log: discardLogger()})
	args, err := l.processArgs(pid)
	if err != nil {
		t.Fatalf("processArgs: %v", err)
	}
	want := []string{"/usr/libexec/qemu-kvm", "-m", "4096", "-name", "guest=el9"}
	if len(args) != len(want) {
		t.Fatalf("got %d args %q, want %d", len(args), args, len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestProcessArgsOnMissingAndEmptyProcess(t *testing.T) {
	proc := t.TempDir()
	l := NewLocal(LocalOptions{ProcRoot: proc, Log: discardLogger()})

	if _, err := l.processArgs(999999); err == nil {
		t.Error("reading a nonexistent process succeeded")
	}

	// A kernel thread has an empty cmdline; that is not a QEMU process.
	os.MkdirAll(filepath.Join(proc, "7"), 0o755)
	os.WriteFile(filepath.Join(proc, "7", "cmdline"), []byte(""), 0o644)
	if _, err := l.processArgs(7); err == nil {
		t.Error("an empty command line was accepted")
	}
}

func TestReadARPMatchesByMAC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arp")
	os.WriteFile(path, []byte(
		"IP address       HW type     Flags       HW address            Mask     Device\n"+
			"10.20.0.50       0x1         0x2         52:54:00:12:34:56     *        br-lab\n"+
			"10.20.0.51       0x1         0x2         52:54:00:AA:BB:CC     *        br-lab\n"+
			"10.20.0.52       0x1         0x0         00:00:00:00:00:00     *        br-lab\n"+
			"10.20.0.53       0x1         0x2         52:54:00:12:34:56     *        br-mgmt\n"), 0o644)

	byMAC, err := readARP(path)
	if err != nil {
		t.Fatalf("readARP: %v", err)
	}
	if got := byMAC["52:54:00:12:34:56"]; len(got) != 2 {
		t.Errorf("addresses for MAC = %v, want two", got)
	}
	// Case in the table must not matter.
	if got := byMAC["52:54:00:aa:bb:cc"]; len(got) != 1 || got[0] != "10.20.0.51" {
		t.Errorf("uppercase MAC not matched: %v", got)
	}
	// Incomplete entries are noise, not addresses.
	if _, ok := byMAC["00:00:00:00:00:00"]; ok {
		t.Error("incomplete ARP entry was reported as an address")
	}
}

func TestEnrichNICsAttachesGuestAddresses(t *testing.T) {
	dir := t.TempDir()
	arp := filepath.Join(dir, "arp")
	os.WriteFile(arp, []byte(
		"IP address       HW type     Flags       HW address            Mask     Device\n"+
			"10.20.0.50       0x1         0x2         52:54:00:12:34:56     *        br-lab\n"), 0o644)

	l := NewLocal(LocalOptions{ArpFile: arp, Log: discardLogger()})
	d := &Details{NICs: []NIC{
		{MAC: "52:54:00:12:34:56"},
		{MAC: "52:54:00:99:99:99"}, // not in the table
		{},                         // no MAC at all
	}}
	l.enrichNICs(d)

	if got := d.NICs[0].Addresses; len(got) != 1 || got[0] != "10.20.0.50" {
		t.Errorf("NIC 0 addresses = %v", got)
	}
	if len(d.NICs[1].Addresses) != 0 {
		t.Errorf("NIC 1 invented addresses: %v", d.NICs[1].Addresses)
	}
	if len(d.NICs[2].Addresses) != 0 {
		t.Errorf("NIC 2 invented addresses: %v", d.NICs[2].Addresses)
	}
}

func TestEnrichNICsSurvivesMissingARPFile(t *testing.T) {
	l := NewLocal(LocalOptions{ArpFile: "/definitely/not/here", Log: discardLogger()})
	d := &Details{NICs: []NIC{{MAC: "52:54:00:12:34:56"}}}
	l.enrichNICs(d) // must not panic; addresses are best effort
	if len(d.NICs[0].Addresses) != 0 {
		t.Error("addresses appeared from nowhere")
	}
}

// A machine with no unit in the inventory is not managed by labview at all,
// and the details page should say so rather than look broken.
func TestInspectWithoutUnitExplainsItself(t *testing.T) {
	l := NewLocal(LocalOptions{ProcRoot: t.TempDir(), ArpFile: "/nope", Log: discardLogger()})
	m := machine(t, "m", "vnc: 10.0.0.1:5901\n")

	d, err := l.Inspect(context.Background(), m)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(d.Warnings) == 0 {
		t.Fatal("no warning for a machine with no unit")
	}
	joined := strings.Join(d.Warnings, " ")
	if !strings.Contains(joined, "no systemd unit") {
		t.Errorf("unhelpful warnings: %v", d.Warnings)
	}
	if d.CommandLine != nil {
		t.Error("invented a command line for an unmanaged machine")
	}
}

func TestUnitStateAndPowerRefuseWithoutUnit(t *testing.T) {
	l := NewLocal(LocalOptions{Log: discardLogger()})
	m := machine(t, "m", "vnc: 10.0.0.1:5901\n")

	var noUnit *ErrNoUnit
	if _, err := l.UnitState(context.Background(), m); !asErrNoUnit(err, &noUnit) {
		t.Errorf("UnitState error = %v, want ErrNoUnit", err)
	}
	// The inventory is the allowlist: no unit means labview refuses rather
	// than guessing a unit name from the id.
	if err := l.Power(context.Background(), m, OpRestart); !asErrNoUnit(err, &noUnit) {
		t.Errorf("Power error = %v, want ErrNoUnit", err)
	}
	if _, err := l.Logs(context.Background(), m, LogOptions{}); !asErrNoUnit(err, &noUnit) {
		t.Errorf("Logs error = %v, want ErrNoUnit", err)
	}
}

func TestPowerRejectsUnknownOperation(t *testing.T) {
	l := NewLocal(LocalOptions{Log: discardLogger()})
	m := machine(t, "m", "unit: qemu-m.service\n")
	err := l.Power(context.Background(), m, Op("poweroff-and-delete"))
	if err == nil {
		t.Fatal("an unknown operation was accepted")
	}
	if !strings.Contains(err.Error(), "unknown power operation") {
		t.Errorf("error = %v", err)
	}
}

// A client names an operation from a fixed set; anything else is refused
// before it can reach systemd.
func TestOpValid(t *testing.T) {
	for _, op := range []Op{OpStart, OpStop, OpRestart} {
		if !op.Valid() {
			t.Errorf("%q should be valid", op)
		}
	}
	for _, op := range []Op{"", "reload", "kill", "mask", "isolate", "START"} {
		if Op(op).Valid() {
			t.Errorf("%q should be rejected", op)
		}
	}
}

// The unit name reaches journalctl as its own argv element, never as part of
// a command string.
func TestJournalArgsPassUnitAsSeparateArgument(t *testing.T) {
	args := journalArgs("qemu-el9.service", LogOptions{Lines: 50}, false)

	idx := -1
	for i, a := range args {
		if a == "--unit" {
			idx = i
		}
	}
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("no --unit in %q", args)
	}
	if args[idx+1] != "qemu-el9.service" {
		t.Errorf("unit argument = %q", args[idx+1])
	}
	if !contains(args, "--lines") || !contains(args, "50") {
		t.Errorf("line limit missing from %q", args)
	}
	if contains(args, "--follow") {
		t.Errorf("non-following read asked to follow: %q", args)
	}
	if got := journalArgs("u.service", LogOptions{}, true); !contains(got, "--follow") {
		t.Errorf("following read did not ask to follow: %q", got)
	}
	// Default line count, so a tab never asks for the whole journal.
	if got := journalArgs("u.service", LogOptions{}, false); !contains(got, "200") {
		t.Errorf("no default line limit: %q", got)
	}
}

func TestJournalMessageHandlesStringAndBinary(t *testing.T) {
	if got := journalMessage("hello"); got != "hello" {
		t.Errorf("string message = %q", got)
	}
	// journalctl renders non-UTF-8 messages as an array of byte values.
	if got := journalMessage([]any{float64(104), float64(105)}); got != "hi" {
		t.Errorf("binary message = %q", got)
	}
	if got := journalMessage(nil); got != "" {
		t.Errorf("nil message = %q", got)
	}
	if got := journalMessage(42); got != "" {
		t.Errorf("unexpected type = %q", got)
	}
}

func TestJournalEntryToLine(t *testing.T) {
	e := journalEntry{
		Message:   "started",
		Priority:  "3",
		Timestamp: "1789574400000000",
		Unit:      "qemu-el9.service",
	}
	l := e.toLine()
	if l.Message != "started" || l.Priority != 3 || l.Unit != "qemu-el9.service" {
		t.Fatalf("line = %+v", l)
	}
	if l.At.IsZero() {
		t.Error("timestamp not parsed")
	}
	// A missing priority must default to informational, not zero (emerg).
	if got := (journalEntry{Message: "x"}).toLine(); got.Priority != 6 {
		t.Errorf("default priority = %d, want 6", got.Priority)
	}
}

func TestUnitRunningAndFailed(t *testing.T) {
	if !(Unit{ActiveState: "active"}).Running() {
		t.Error("active unit not reported running")
	}
	if (Unit{ActiveState: "inactive"}).Running() {
		t.Error("inactive unit reported running")
	}
	// A crashed machine reports as failed rather than merely absent, which
	// is the reason section 12 chose systemd over QMP.
	if !(Unit{ActiveState: "failed"}).Failed() {
		t.Error("failed unit not reported failed")
	}
	if !(Unit{ActiveState: "inactive", Result: "exit-code"}).Failed() {
		t.Error("unit with a failure result not reported failed")
	}
	if (Unit{ActiveState: "active", Result: "success"}).Failed() {
		t.Error("healthy unit reported failed")
	}
}

func asErrNoUnit(err error, target **ErrNoUnit) bool {
	if err == nil {
		return false
	}
	e, ok := err.(*ErrNoUnit)
	if ok {
		*target = e
	}
	return ok
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
