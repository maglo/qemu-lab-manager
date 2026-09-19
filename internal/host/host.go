// Package host is labview's window onto the hypervisor it runs on.
//
// It exists because the design's channels split two ways: the framebuffer and
// the serial line are dialable over the network, while the QEMU command line,
// the unit state and power operations are local to the hypervisor (design
// section 13, "Host access out").
//
// Everything here is readable without libvirt and without an agent, which is
// the point. The interface is also the seam that a future aggregating labview
// would implement by speaking the section 5 API to another labview.
package host

import (
	"context"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

// Op is a power operation. Start, stop and restart go through systemd rather
// than QMP, because the VMs are systemd units and the unit reports a crashed
// machine as failed rather than merely absent (design section 12).
type Op string

const (
	OpStart   Op = "start"
	OpStop    Op = "stop"
	OpRestart Op = "restart"
)

// Valid reports whether op is one labview performs. A client names one of
// these and a machine id; it never names a unit.
func (o Op) Valid() bool {
	switch o {
	case OpStart, OpStop, OpRestart:
		return true
	}
	return false
}

// Unit is what systemd says about a machine's unit.
type Unit struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	LoadState   string    `json:"loadState,omitempty"`
	ActiveState string    `json:"activeState,omitempty"`
	SubState    string    `json:"subState,omitempty"`
	Since       time.Time `json:"since,omitzero"`
	MainPID     int       `json:"mainPid,omitempty"`
	Result      string    `json:"result,omitempty"`
}

// Running reports whether the unit is active. A failed unit is not running
// but is very much worth showing, which is why Failed is separate.
func (u Unit) Running() bool { return u.ActiveState == "active" }

// Failed reports whether systemd considers the unit failed.
//
// Result belongs to the run that ended, not to the state the unit is in now.
// A unit with Restart=on-failure passes through activating/auto-restart on its
// way back up, and it carries the previous run's Result through it, so Result
// alone calls a machine that is coming back a failure.
func (u Unit) Failed() bool {
	if u.Starting() {
		return false
	}
	return u.ActiveState == "failed" || u.Result != "" && u.Result != "success"
}

// Starting reports whether the unit is on its way up, which includes the
// pause that Restart= puts between two runs.
func (u Unit) Starting() bool { return u.ActiveState == "activating" }

// Disk describes one backing image.
type Disk struct {
	Path        string `json:"path"`
	Format      string `json:"format,omitempty"`
	Interface   string `json:"interface,omitempty"`
	VirtualSize int64  `json:"virtualSize,omitempty"`
	ActualSize  int64  `json:"actualSize,omitempty"`
	BackingFile string `json:"backingFile,omitempty"`
	ReadOnly    bool   `json:"readOnly,omitempty"`
	Error       string `json:"error,omitempty"`
}

// NIC describes one guest network interface.
type NIC struct {
	Kind     string `json:"kind,omitempty"`   // tap, user, bridge
	Tap      string `json:"tap,omitempty"`    // host side interface
	Bridge   string `json:"bridge,omitempty"` // bridge it is enslaved to
	MAC      string `json:"mac,omitempty"`
	Model    string `json:"model,omitempty"`
	NetdevID string `json:"netdevId,omitempty"`
	// Addresses are guest addresses, when the host happens to know them --
	// learned from its own neighbour table, not from an agent in the guest.
	Addresses []string `json:"addresses,omitempty"`
}

// Hardware is the static shape of the machine.
type Hardware struct {
	MemoryMB    int64  `json:"memoryMb,omitempty"`
	VCPUs       int    `json:"vcpus,omitempty"`
	MachineType string `json:"machineType,omitempty"`
	Firmware    string `json:"firmware,omitempty"`
	CPUModel    string `json:"cpuModel,omitempty"`
}

// Details is everything static or slow-moving about a machine, as the details
// tab shows it (design section 8). Every field here is also reachable as JSON
// through the API: nothing appears in the UI that a harness cannot fetch.
type Details struct {
	// CommandLine is the full QEMU invocation, as invoked. The design calls
	// this the single most useful thing on the page and the most annoying
	// to dig out by hand.
	CommandLine []string `json:"commandLine,omitempty"`

	Unit     Unit     `json:"unit"`
	Hardware Hardware `json:"hardware"`
	Disks    []Disk   `json:"disks,omitempty"`
	NICs     []NIC    `json:"nics,omitempty"`

	// Warnings records what could not be gathered and why, so a partial
	// page is honest about being partial instead of silently omitting
	// things.
	Warnings []string `json:"warnings,omitempty"`
}

// Access is labview's host-side introspection and control.
//
// Implementations must treat a machine as opaque: they receive the inventory
// entry and map it to a unit themselves. That mapping is what makes the
// inventory the allowlist -- labview can touch exactly the units it was told
// about and nothing else on the host (design section 12).
type Access interface {
	// Inspect gathers the details tab's contents. It returns partial
	// Details with Warnings rather than failing outright, because a VM that
	// is switched off still has a unit and an inventory entry worth showing.
	Inspect(ctx context.Context, m inventory.Machine) (Details, error)

	// UnitState is the cheap subset of Inspect, for the machines listing.
	UnitState(ctx context.Context, m inventory.Machine) (Unit, error)

	// Power performs a power operation. The caller has already checked the
	// write lease; this call does not know about leases.
	Power(ctx context.Context, m inventory.Machine, op Op) error

	// Close releases any host resources, such as a bus connection.
	Close() error
}

// ErrNoUnit is returned when an operation needs a unit and the inventory
// entry has none. The inventory is the allowlist, so a missing unit is a
// refusal rather than a lookup failure.
type ErrNoUnit struct{ Machine string }

func (e *ErrNoUnit) Error() string {
	return "machine " + e.Machine + " has no systemd unit in the inventory, so labview will not manage it"
}

// ErrUnsupported is returned by implementations that cannot reach the host,
// such as when labview runs somewhere other than the hypervisor.
type ErrUnsupported struct{ What, Why string }

func (e *ErrUnsupported) Error() string {
	if e.Why == "" {
		return e.What + " is not available on this host"
	}
	return e.What + " is not available: " + e.Why
}
