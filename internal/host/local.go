package host

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

const (
	systemdDest = "org.freedesktop.systemd1"
	systemdPath = dbus.ObjectPath("/org/freedesktop/systemd1")
	systemdMgr  = "org.freedesktop.systemd1.Manager"
)

// LocalOptions configures the hypervisor-local implementation.
type LocalOptions struct {
	// QEMUImgPath is the qemu-img binary used for disk sizes. Empty looks
	// it up on PATH; disk sizes are simply omitted if it is absent.
	QEMUImgPath string

	// ProcRoot and ArpFile are overridable for tests.
	ProcRoot string
	ArpFile  string

	// CommandTimeout bounds each helper invocation.
	CommandTimeout time.Duration

	Log *slog.Logger
}

// Local reads and controls VMs on the hypervisor labview runs on.
//
// Nothing here needs libvirt or an in-guest agent: the unit, its cgroup and
// its command line are all readable from the host (design section 8).
type Local struct {
	opts LocalOptions
	log  *slog.Logger

	busOnce sync.Once
	busMu   sync.Mutex
	bus     *dbus.Conn
	busErr  error
}

// NewLocal returns host access for the local hypervisor.
//
// It does not connect to the system bus here. labview should start and serve
// the wall even on a machine with no systemd reachable -- the affected tabs
// then report why they are empty, which is more useful than refusing to boot.
func NewLocal(opts LocalOptions) *Local {
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 10 * time.Second
	}
	if opts.ProcRoot == "" {
		opts.ProcRoot = "/proc"
	}
	if opts.ArpFile == "" {
		opts.ArpFile = "/proc/net/arp"
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Local{opts: opts, log: opts.Log}
}

// systemBus connects lazily and caches the result.
func (l *Local) systemBus() (*dbus.Conn, error) {
	l.busMu.Lock()
	defer l.busMu.Unlock()
	if l.bus != nil {
		if l.bus.Connected() {
			return l.bus, nil
		}
		l.bus = nil
	}
	conn, err := dbus.SystemBus()
	if err != nil {
		l.busErr = err
		return nil, &ErrUnsupported{What: "systemd", Why: err.Error()}
	}
	l.bus = conn
	l.busErr = nil
	return conn, nil
}

// Close releases the bus connection. The shared system bus is reference
// counted by the library, so this is a Close and not a Disconnect.
func (l *Local) Close() error {
	l.busMu.Lock()
	defer l.busMu.Unlock()
	if l.bus == nil {
		return nil
	}
	err := l.bus.Close()
	l.bus = nil
	return err
}

// unitObject loads a unit and returns its object.
//
// LoadUnit rather than GetUnit: a machine that is switched off has no loaded
// unit, and "switched off" is exactly the state a developer wants to look at
// before starting it.
func (l *Local) unitObject(ctx context.Context, unit string) (dbus.BusObject, error) {
	conn, err := l.systemBus()
	if err != nil {
		return nil, err
	}
	mgr := conn.Object(systemdDest, systemdPath)

	var path dbus.ObjectPath
	if err := mgr.CallWithContext(ctx, systemdMgr+".LoadUnit", 0, unit).Store(&path); err != nil {
		return nil, fmt.Errorf("load unit %s: %w", unit, err)
	}
	return conn.Object(systemdDest, path), nil
}

// UnitState reports what systemd says about a machine's unit.
func (l *Local) UnitState(ctx context.Context, m inventory.Machine) (Unit, error) {
	if !m.CanPower() {
		return Unit{}, &ErrNoUnit{Machine: m.ID}
	}
	obj, err := l.unitObject(ctx, m.Unit)
	if err != nil {
		return Unit{Name: m.Unit}, err
	}

	u := Unit{Name: m.Unit}
	u.Description = l.stringProp(obj, "org.freedesktop.systemd1.Unit.Description")
	u.LoadState = l.stringProp(obj, "org.freedesktop.systemd1.Unit.LoadState")
	u.ActiveState = l.stringProp(obj, "org.freedesktop.systemd1.Unit.ActiveState")
	u.SubState = l.stringProp(obj, "org.freedesktop.systemd1.Unit.SubState")
	u.Result = l.stringProp(obj, "org.freedesktop.systemd1.Service.Result")

	// StateChangeTimestamp is microseconds since the epoch, and is zero for
	// a unit that has never changed state.
	if usec := l.uint64Prop(obj, "org.freedesktop.systemd1.Unit.StateChangeTimestamp"); usec > 0 {
		u.Since = time.UnixMicro(int64(usec))
	}
	u.MainPID = int(l.uint32Prop(obj, "org.freedesktop.systemd1.Service.MainPID"))
	return u, nil
}

func (l *Local) stringProp(obj dbus.BusObject, name string) string {
	v, err := obj.GetProperty(name)
	if err != nil {
		return ""
	}
	s, _ := v.Value().(string)
	return s
}

func (l *Local) uint64Prop(obj dbus.BusObject, name string) uint64 {
	v, err := obj.GetProperty(name)
	if err != nil {
		return 0
	}
	n, _ := v.Value().(uint64)
	return n
}

func (l *Local) uint32Prop(obj dbus.BusObject, name string) uint32 {
	v, err := obj.GetProperty(name)
	if err != nil {
		return 0
	}
	n, _ := v.Value().(uint32)
	return n
}

// Power starts, stops or restarts a machine's unit over D-Bus.
//
// Never by shelling out to systemctl: that would mean building a unit name
// into a command string from a request, and one parsing mistake there is
// command execution on the hypervisor (design section 12). The unit name also
// comes from the inventory and was validated at load, so a request can only
// ever select from the units labview was told about.
func (l *Local) Power(ctx context.Context, m inventory.Machine, op Op) error {
	if !op.Valid() {
		return fmt.Errorf("unknown power operation %q", op)
	}
	if !m.CanPower() {
		return &ErrNoUnit{Machine: m.ID}
	}

	conn, err := l.systemBus()
	if err != nil {
		return err
	}
	mgr := conn.Object(systemdDest, systemdPath)

	var method string
	switch op {
	case OpStart:
		method = systemdMgr + ".StartUnit"
	case OpStop:
		method = systemdMgr + ".StopUnit"
	case OpRestart:
		method = systemdMgr + ".RestartUnit"
	}

	// "replace" is systemd's usual job mode: supersede any queued job for
	// this unit rather than failing or stacking up.
	var job dbus.ObjectPath
	if err := mgr.CallWithContext(ctx, method, 0, m.Unit, "replace").Store(&job); err != nil {
		return fmt.Errorf("%s %s: %w", op, m.Unit, err)
	}
	l.log.Info("power operation submitted",
		"machine", m.ID, "unit", m.Unit, "op", string(op), "job", string(job))
	return nil
}

// Inspect gathers the details tab.
//
// It is deliberately forgiving: a switched-off machine, a missing qemu-img, an
// unreachable bus each cost one field and one warning rather than the whole
// page.
func (l *Local) Inspect(ctx context.Context, m inventory.Machine) (Details, error) {
	var d Details

	if m.CanPower() {
		u, err := l.UnitState(ctx, m)
		d.Unit = u
		if err != nil {
			d.Warnings = append(d.Warnings, "unit state unavailable: "+err.Error())
		}
	} else {
		d.Warnings = append(d.Warnings,
			"no systemd unit in the inventory, so unit state, logs and power operations are unavailable")
	}

	// The command line comes from the running process, so it is only
	// available while the machine is up.
	if d.Unit.MainPID > 0 {
		args, err := l.processArgs(d.Unit.MainPID)
		if err != nil {
			d.Warnings = append(d.Warnings, "command line unavailable: "+err.Error())
		} else {
			d.CommandLine = args
			d.Hardware, d.Disks, d.NICs = ParseQEMUArgs(args)
		}
	} else if m.CanPower() {
		d.Warnings = append(d.Warnings,
			"machine is not running, so its command line, disks and interfaces are not known")
	}

	l.enrichDisks(ctx, &d)
	l.enrichNICs(&d)
	return d, nil
}

// processArgs reads a process's argv from /proc, which is where the command
// line as actually invoked lives.
func (l *Local) processArgs(pid int) ([]string, error) {
	path := fmt.Sprintf("%s/%d/cmdline", l.opts.ProcRoot, pid)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// argv is NUL separated, with a trailing NUL.
	parts := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(parts) == 1 && parts[0] == "" {
		return nil, fmt.Errorf("process %d has an empty command line", pid)
	}
	return parts, nil
}

// qemuImgInfo is the subset of qemu-img's JSON output that the details tab
// shows.
type qemuImgInfo struct {
	VirtualSize int64  `json:"virtual-size"`
	ActualSize  int64  `json:"actual-size"`
	Format      string `json:"format"`
	BackingFile string `json:"backing-filename"`
}

// enrichDisks fills in sizes and backing files. Reading an image that a VM is
// writing to is safe: qemu-img info opens it read only.
func (l *Local) enrichDisks(ctx context.Context, d *Details) {
	if len(d.Disks) == 0 {
		return
	}
	bin := l.opts.QEMUImgPath
	if bin == "" {
		var err error
		bin, err = exec.LookPath("qemu-img")
		if err != nil {
			d.Warnings = append(d.Warnings, "qemu-img not found, so disk sizes are unavailable")
			return
		}
	}

	for i := range d.Disks {
		disk := &d.Disks[i]
		cctx, cancel := context.WithTimeout(ctx, l.opts.CommandTimeout)
		// argv, not a shell: the path comes from the command line of a
		// running process and is never interpolated into a string.
		out, err := exec.CommandContext(cctx, bin, "info", "--output=json", "--force-share", disk.Path).Output()
		cancel()
		if err != nil {
			disk.Error = "qemu-img info failed: " + firstLine(err.Error())
			continue
		}
		var info qemuImgInfo
		if err := json.Unmarshal(out, &info); err != nil {
			disk.Error = "qemu-img output not understood"
			continue
		}
		disk.VirtualSize = info.VirtualSize
		disk.ActualSize = info.ActualSize
		disk.BackingFile = info.BackingFile
		if disk.Format == "" {
			disk.Format = info.Format
		}
	}
}

// enrichNICs adds guest addresses the host already knows, from its own
// neighbour table. "Guest addresses if known" (design section 8) -- known to
// the host, that is; there is no agent in the guest to ask.
func (l *Local) enrichNICs(d *Details) {
	if len(d.NICs) == 0 {
		return
	}
	byMAC, err := readARP(l.opts.ArpFile)
	if err != nil {
		return // not worth a warning; addresses are explicitly best effort
	}
	for i := range d.NICs {
		mac := strings.ToLower(d.NICs[i].MAC)
		if mac == "" {
			continue
		}
		d.NICs[i].Addresses = byMAC[mac]
	}
}

// readARP parses the kernel's IPv4 neighbour table, keyed by lowercase MAC.
func readARP(path string) (map[string][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string][]string{}
	sc := bufio.NewScanner(f)
	if sc.Scan() { // header
		_ = sc.Text()
	}
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// IP address, HW type, Flags, HW address, Mask, Device
		if len(fields) < 4 {
			continue
		}
		ip, mac := fields[0], strings.ToLower(fields[3])
		if mac == "00:00:00:00:00:00" {
			continue // incomplete entry
		}
		out[mac] = append(out[mac], ip)
	}
	return out, sc.Err()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Local implements Access.
var _ Access = (*Local)(nil)
