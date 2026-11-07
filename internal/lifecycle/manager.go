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
	firmware     FirmwarePaths
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
	// Firmware locates OVMF files. Empty uses DefaultFirmwarePaths.
	Firmware FirmwarePaths
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
	fw := opts.Firmware
	if fw.Code == "" && fw.VarsTemplate == "" {
		fw = DefaultFirmwarePaths()
	}
	return &Manager{
		machinesDir:  opts.MachinesDir,
		inventoryDir: opts.InventoryDir,
		inventory:    opts.Inventory,
		firmware:     fw,
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

	disks, err := spec.ResolvedDisks()
	if err != nil {
		return inventory.Machine{}, err
	}
	if err := prepareDisks(dir, disks); err != nil {
		return inventory.Machine{}, err
	}
	if spec.FirmwareNorm() == FirmwareUEFI {
		if err := prepareUEFIVars(dir, m.firmware); err != nil {
			return inventory.Machine{}, err
		}
	}
	if spec.TPM != nil {
		if err := prepareTPMState(dir); err != nil {
			return inventory.Machine{}, err
		}
	}

	vncDisplay, err := allocateVNCPort(m.machinesDir, spec.ID)
	if err != nil {
		return inventory.Machine{}, err
	}
	vncPort := vncBasePort + vncDisplay
	serialPath := filepath.Join(dir, "serial.sock")
	qmpPath := filepath.Join(dir, "qmp.sock")
	vnc := fmt.Sprintf("127.0.0.1:%d", vncPort)

	lines, err := buildArgv(spec, dir, serialPath, qmpPath, vncDisplay, m.firmware)
	if err != nil {
		return inventory.Machine{}, err
	}
	if err := writeArgv(filepath.Join(dir, "qemu.argv"), lines); err != nil {
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
	m.log.Info("machine created", "id", spec.ID, "unit", mach.Unit, "vnc", vnc,
		"firmware", spec.FirmwareNorm())
	return mach, nil
}

// PatchResult is the outcome of a desired-state update.
type PatchResult struct {
	Spec         Spec `json:"desired"`
	NeedsRestart bool `json:"needsRestart"`
}

// Patch updates desired state, rewrites qemu.argv, and reports whether
// the unit must restart for the change to take effect.
func (m *Manager) Patch(ctx context.Context, id string, patch PatchSpec) (PatchResult, error) {
	if !m.Enabled() {
		return PatchResult{}, ErrDisabled
	}
	if err := validateID(id); err != nil {
		return PatchResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	stored, err := m.readStored(id)
	if err != nil {
		return PatchResult{}, err
	}
	next := stored.Spec.Apply(patch)
	next.ID = id
	if err := next.Validate(); err != nil {
		return PatchResult{}, err
	}

	dir := m.machineDir(id)
	disks, err := next.ResolvedDisks()
	if err != nil {
		return PatchResult{}, err
	}
	if err := prepareDisks(dir, disks); err != nil {
		return PatchResult{}, err
	}
	if next.FirmwareNorm() == FirmwareUEFI {
		if err := prepareUEFIVars(dir, m.firmware); err != nil {
			return PatchResult{}, err
		}
	}
	if next.TPM != nil {
		if err := prepareTPMState(dir); err != nil {
			return PatchResult{}, err
		}
	}

	serialPath := filepath.Join(dir, "serial.sock")
	qmpPath := filepath.Join(dir, "qmp.sock")
	vncDisplay := stored.VNCPort - vncBasePort
	if vncDisplay < 0 {
		vncDisplay = 0
	}
	lines, err := buildArgv(next, dir, serialPath, qmpPath, vncDisplay, m.firmware)
	if err != nil {
		return PatchResult{}, err
	}
	if err := writeArgv(filepath.Join(dir, "qemu.argv"), lines); err != nil {
		return PatchResult{}, err
	}
	if err := writeSpecFile(filepath.Join(dir, "spec.json"), next, stored.VNCPort); err != nil {
		return PatchResult{}, err
	}

	// Name/notes also live in the inventory file the wall reads.
	if mach, ok := m.inventory.Current().Get(id); ok {
		mach.Name = next.DisplayName()
		mach.Notes = next.Notes
		if err := writeInventoryFile(m.inventoryDir, mach); err != nil {
			return PatchResult{}, err
		}
		m.inventory.Rescan()
	}

	needsRestart := desiredNeedsRestart(stored.Spec, next)
	m.log.Info("machine patched", "id", id, "needsRestart", needsRestart)
	return PatchResult{Spec: next, NeedsRestart: needsRestart}, nil
}

// Desired returns the stored Spec for a managed machine.
func (m *Manager) Desired(id string) (Spec, error) {
	if !m.Enabled() {
		return Spec{}, ErrDisabled
	}
	stored, err := m.readStored(id)
	if err != nil {
		return Spec{}, err
	}
	return stored.Spec, nil
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

func (m *Manager) readStored(id string) (storedSpec, error) {
	path := filepath.Join(m.machineDir(id), "spec.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return storedSpec{}, fmt.Errorf("%w: %s", ErrNotManaged, id)
		}
		return storedSpec{}, err
	}
	var stored storedSpec
	if err := json.Unmarshal(b, &stored); err != nil {
		return storedSpec{}, fmt.Errorf("spec.json: %w", err)
	}
	if stored.ID == "" {
		stored.ID = id
	}
	return stored, nil
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

func desiredNeedsRestart(before, after Spec) bool {
	if before.MemoryMB != after.MemoryMB || before.VCPUs != after.VCPUs {
		return true
	}
	if before.FirmwareNorm() != after.FirmwareNorm() {
		return true
	}
	beforeDisks, _ := before.ResolvedDisks()
	afterDisks, _ := after.ResolvedDisks()
	if len(beforeDisks) != len(afterDisks) {
		return true
	}
	for i := range beforeDisks {
		if beforeDisks[i].Name != afterDisks[i].Name ||
			beforeDisks[i].Image != afterDisks[i].Image ||
			beforeDisks[i].BusNorm() != afterDisks[i].BusNorm() ||
			beforeDisks[i].SizeGB != afterDisks[i].SizeGB {
			return true
		}
	}
	if (before.TPM == nil) != (after.TPM == nil) {
		return true
	}
	if before.TPM != nil && after.TPM != nil &&
		(before.TPM.VersionNorm() != after.TPM.VersionNorm() ||
			before.TPM.ModelNorm() != after.TPM.ModelNorm()) {
		return true
	}
	if before.USBControllers() != after.USBControllers() {
		return true
	}
	if len(before.CCID) != len(after.CCID) {
		return true
	}
	return false
}
