package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

// ErrDisabled means machine LCM is not configured on this labview.
var ErrDisabled = errors.New("machine lifecycle is disabled on this labview")

// ErrExists means create named a machine that is already there.
var ErrExists = errors.New("machine already exists")

// ErrNotManaged means destroy named a machine this manager did not create.
var ErrNotManaged = errors.New("machine was not created by the lifecycle API")

// Inventory is the live machine set the wall reads.
type Inventory interface {
	Current() *inventory.Set
	// Rescan reads the inventory directory again now.
	Rescan()
}

// Manager creates and destroys machines on one node.
type Manager struct {
	machinesDir  string
	inventoryDir string
	inventory    Inventory
	// stop, if set, stops the unit before files go away.
	stop func(ctx context.Context, m inventory.Machine) error
	log  *slog.Logger

	mu sync.Mutex
}

// Options configures a Manager.
type Options struct {
	// MachinesDir holds per-machine disks and QEMU argv files. Empty
	// disables LCM.
	MachinesDir string
	// InventoryDir receives the YAML file create writes.
	InventoryDir string
	Inventory    Inventory
	// Stop stops a machine's unit. Nil skips the stop (tests that only
	// check files).
	Stop func(ctx context.Context, m inventory.Machine) error
	Log  *slog.Logger
}

// New returns a Manager. A missing MachinesDir disables Create and Destroy.
func New(opts Options) *Manager {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		machinesDir:  opts.MachinesDir,
		inventoryDir: opts.InventoryDir,
		inventory:    opts.Inventory,
		stop:         opts.Stop,
		log:          log,
	}
}

// Enabled reports whether Create and Destroy are available.
func (m *Manager) Enabled() bool {
	return m != nil && m.machinesDir != "" && m.inventoryDir != "" && m.inventory != nil
}

// Create provisions a machine and writes its inventory entry.
func (m *Manager) Create(ctx context.Context, spec Spec) (inventory.Machine, error) {
	if !m.Enabled() {
		return inventory.Machine{}, ErrDisabled
	}
	if err := spec.Validate(); err != nil {
		return inventory.Machine{}, err
	}
	if _, ok := m.inventory.Current().Get(spec.ID); ok {
		return inventory.Machine{}, fmt.Errorf("%w: %s", ErrExists, spec.ID)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	dir := m.machineDir(spec.ID)
	if _, err := os.Stat(dir); err == nil {
		return inventory.Machine{}, fmt.Errorf("%w: %s", ErrExists, spec.ID)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return inventory.Machine{}, fmt.Errorf("create machine dir: %w", err)
	}

	rollback := true
	defer func() {
		if rollback {
			_ = os.RemoveAll(dir)
			_ = removeInventoryFile(m.inventoryDir, spec.ID)
		}
	}()

	diskPath := filepath.Join(dir, "disk.raw")
	if err := createSparseDisk(diskPath, spec.DiskGB<<30); err != nil {
		return inventory.Machine{}, err
	}

	vncDisplay, err := allocateVNCPort(m.machinesDir, spec.ID)
	if err != nil {
		return inventory.Machine{}, err
	}
	vncPort := vncBasePort + vncDisplay
	serialPath := filepath.Join(dir, "serial.sock")
	qmpPath := filepath.Join(dir, "qmp.sock")
	vnc := fmt.Sprintf("127.0.0.1:%d", vncPort)

	if err := writeArgv(filepath.Join(dir, "qemu.argv"), spec, diskPath, serialPath, qmpPath, vncDisplay); err != nil {
		return inventory.Machine{}, err
	}
	if err := writeSpecFile(filepath.Join(dir, "spec.json"), spec, vncPort); err != nil {
		return inventory.Machine{}, err
	}

	mach := inventory.Machine{
		ID:      spec.ID,
		Name:    spec.DisplayName(),
		Notes:   spec.Notes,
		VNC:     vnc,
		Serial:  serialPath,
		Control: qmpPath,
		Unit:    spec.UnitName(),
	}
	if err := writeInventoryFile(m.inventoryDir, mach); err != nil {
		return inventory.Machine{}, err
	}

	rollback = false
	m.inventory.Rescan()
	m.log.Info("machine created", "id", spec.ID, "unit", mach.Unit, "vnc", vnc)
	return mach, nil
}

// Destroy stops the machine and removes its files and inventory entry.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	if !m.Enabled() {
		return ErrDisabled
	}
	if err := validateID(id); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	dir := m.machineDir(id)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrNotManaged, id)
		}
		return err
	}

	if mach, ok := m.inventory.Current().Get(id); ok && m.stop != nil && mach.CanPower() {
		if err := m.stop(ctx, mach); err != nil {
			m.log.Warn("stop before destroy failed", "id", id, "error", err)
			// Continue: a missing unit should not block cleanup.
		}
	}

	if err := removeInventoryFile(m.inventoryDir, id); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove machine dir: %w", err)
	}
	m.inventory.Rescan()
	m.log.Info("machine destroyed", "id", id)
	return nil
}

// Managed reports whether id was created through this manager.
func (m *Manager) Managed(id string) bool {
	if !m.Enabled() {
		return false
	}
	_, err := os.Stat(m.machineDir(id))
	return err == nil
}

func (m *Manager) machineDir(id string) string {
	return filepath.Join(m.machinesDir, id)
}

type storedSpec struct {
	Spec
	VNCPort int `json:"vncPort"`
}

func writeSpecFile(path string, spec Spec, vncPort int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(storedSpec{Spec: spec, VNCPort: vncPort})
}

func writeArgv(path string, spec Spec, disk, serial, qmp string, vncDisplay int) error {
	// systemd "@file" argv format: one argument per line.
	lines := []string{
		"-name", spec.ID,
		"-machine", "q35,accel=kvm:tcg",
		"-cpu", "max",
		"-m", fmt.Sprintf("%d", spec.MemoryMB),
		"-smp", fmt.Sprintf("%d", spec.VCPUs),
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,cache=writeback", disk),
		"-display", "none",
		"-vnc", fmt.Sprintf("127.0.0.1:%d", vncDisplay),
		"-chardev", fmt.Sprintf("socket,id=serial0,path=%s,server=on,wait=off", serial),
		"-serial", "chardev:serial0",
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", qmp),
		"-nodefaults",
	}
	if spec.Image != "" {
		// Backing image support lands with a later slice; refuse for now
		// rather than silently ignore.
		return errors.New("image backing files are not supported in this slice")
	}
	var b []byte
	for _, line := range lines {
		b = append(b, line...)
		b = append(b, '\n')
	}
	return os.WriteFile(path, b, 0o640)
}
