package host

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

// Fake is host access with no hypervisor behind it.
//
// It exists for two reasons: the API layer needs something deterministic to
// test against, and labview should be runnable on a developer's laptop to work
// on the UI without a lab. It is selected explicitly by configuration, never
// fallen back to -- a wall that silently shows invented machine details would
// be worse than one that says it cannot reach the host.
type Fake struct {
	mu    sync.Mutex
	units map[string]Unit
	ops   []string

	// PowerErr, if set, is returned by Power, for testing the failure path.
	PowerErr error
	// InspectErr likewise.
	InspectErr error
	// LogLines is what Logs returns.
	LogLines []LogLine
}

// NewFake returns fake host access with every machine stopped.
func NewFake() *Fake {
	return &Fake{units: make(map[string]Unit)}
}

// SetUnit forces a machine's unit state.
func (f *Fake) SetUnit(machineID string, u Unit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.units[machineID] = u
}

// Ops returns the power operations performed, as "machine:op" strings, so a
// test can assert that a refused request never reached the host.
func (f *Fake) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func (f *Fake) unit(m inventory.Machine) Unit {
	if u, ok := f.units[m.ID]; ok {
		if u.Name == "" {
			u.Name = m.Unit
		}
		return u
	}
	return Unit{
		Name:        m.Unit,
		Description: m.DisplayName(),
		LoadState:   "loaded",
		ActiveState: "inactive",
		SubState:    "dead",
	}
}

// UnitState implements Access.
func (f *Fake) UnitState(_ context.Context, m inventory.Machine) (Unit, error) {
	if !m.CanPower() {
		return Unit{}, &ErrNoUnit{Machine: m.ID}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unit(m), nil
}

// Inspect implements Access.
func (f *Fake) Inspect(_ context.Context, m inventory.Machine) (Details, error) {
	if f.InspectErr != nil {
		return Details{}, f.InspectErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	d := Details{}
	if !m.CanPower() {
		d.Warnings = append(d.Warnings,
			"no systemd unit in the inventory, so unit state, logs and power operations are unavailable")
		return d, nil
	}
	d.Unit = f.unit(m)
	if !d.Unit.Running() {
		d.Warnings = append(d.Warnings,
			"machine is not running, so its command line, disks and interfaces are not known")
		return d, nil
	}

	d.CommandLine = []string{
		"/usr/libexec/qemu-kvm",
		"-name", "guest=" + m.ID,
		"-machine", "pc-q35-rhel9.2.0",
		"-cpu", "host",
		"-m", "4096",
		"-smp", "4",
		"-drive", fmt.Sprintf("file=/var/lib/libvirt/images/%s.qcow2,format=qcow2,if=virtio", m.ID),
		"-netdev", "tap,id=net0,ifname=tap-" + m.ID + ",br=br-lab",
		"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56",
	}
	d.Hardware, d.Disks, d.NICs = ParseQEMUArgs(d.CommandLine)
	d.Warnings = append(d.Warnings, "host access is faked; these details are not from a real hypervisor")
	return d, nil
}

// Power implements Access.
func (f *Fake) Power(_ context.Context, m inventory.Machine, op Op) error {
	if !op.Valid() {
		return fmt.Errorf("unknown power operation %q", op)
	}
	if !m.CanPower() {
		return &ErrNoUnit{Machine: m.ID}
	}
	if f.PowerErr != nil {
		return f.PowerErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, m.ID+":"+string(op))

	u := f.unit(m)
	u.Since = time.Now()
	switch op {
	case OpStart, OpRestart:
		u.ActiveState, u.SubState, u.MainPID = "active", "running", 4242
	case OpStop:
		u.ActiveState, u.SubState, u.MainPID = "inactive", "dead", 0
	}
	f.units[m.ID] = u
	return nil
}

// Logs implements Access.
func (f *Fake) Logs(_ context.Context, m inventory.Machine, _ LogOptions) ([]LogLine, error) {
	if !m.CanPower() {
		return nil, &ErrNoUnit{Machine: m.ID}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.LogLines != nil {
		return append([]LogLine(nil), f.LogLines...), nil
	}
	return []LogLine{{
		At:       time.Now(),
		Priority: 6,
		Unit:     m.Unit,
		Message:  "host access is faked; there is no journal to read",
	}}, nil
}

// TailLogs implements Access. It replays Logs once and then waits for the
// context, which is enough for the UI to render.
func (f *Fake) TailLogs(ctx context.Context, m inventory.Machine) (<-chan LogLine, error) {
	lines, err := f.Logs(ctx, m, LogOptions{})
	if err != nil {
		return nil, err
	}
	ch := make(chan LogLine, len(lines)+1)
	for _, l := range lines {
		ch <- l
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// Close implements Access.
func (f *Fake) Close() error { return nil }

// Fake implements Access.
var _ Access = (*Fake)(nil)
