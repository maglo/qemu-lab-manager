// Package lifecycle creates and destroys machines on one node
// (docs/design/cluster.md section 4).
//
// The API is the source of machine truth. A create writes the disk, the
// QEMU argv file and an inventory entry. A destroy stops the unit and
// removes those files. Power still goes through systemd.
//
// Desired hardware (firmware, disks, TPM, USB, CCID) lives on Spec.
// Timed actions and asserts stay in the scenario runner
// (docs/design/scenarios.md).
package lifecycle

import (
	"errors"
	"fmt"
	"strings"
)

// Firmware values.
const (
	FirmwareBIOS = "bios"
	FirmwareUEFI = "uefi"
)

// Spec is the create and PATCH document for one machine.
//
// Scenario documents pass the same shape under
// `spec.machines[].create` (docs/design/scenarios.md).
type Spec struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Notes    string `json:"notes,omitempty"`
	MemoryMB int64  `json:"memoryMb"`
	VCPUs    int    `json:"vcpus"`
	// DiskGB is the size of the primary disk when Disks is empty.
	DiskGB int64 `json:"diskGb,omitempty"`
	// Image is an existing disk file for the primary disk when Disks
	// is empty. Empty means a blank sparse disk of DiskGB.
	Image string `json:"image,omitempty"`

	// Firmware is "bios" (default) or "uefi" (OVMF).
	Firmware string `json:"firmware,omitempty"`
	// Disks declares one or more disks. Empty falls back to DiskGB/Image.
	Disks []DiskSpec `json:"disks,omitempty"`
	// TPM, when set, attaches an emulator-backed TPM.
	TPM *TPMSpec `json:"tpm,omitempty"`
	// USB declares the USB controller and optional boot devices.
	USB *USBSpec `json:"usb,omitempty"`
	// CCID declares smart-card readers present at boot.
	CCID []CCIDSpec `json:"ccid,omitempty"`
}

// DiskSpec is one declared disk.
type DiskSpec struct {
	// Name is the file stem under the machine directory. Empty means
	// "disk" for the first entry and "diskN" after that.
	Name string `json:"name,omitempty"`
	// SizeGB sizes a blank disk. Ignored when Image is set.
	SizeGB int64 `json:"sizeGb,omitempty"`
	// Image is an existing disk file to attach. Empty creates a blank
	// sparse raw disk of SizeGB.
	Image string `json:"image,omitempty"`
	// Bus is virtio (default), scsi or ide.
	Bus string `json:"bus,omitempty"`
	// ReadOnly attaches the image read-only.
	ReadOnly bool `json:"readOnly,omitempty"`
}

// TPMSpec declares a virtual TPM.
type TPMSpec struct {
	// Version is "2.0" (default) or "1.2".
	Version string `json:"version,omitempty"`
	// Model is "tpm-tis" (default) or "tpm-crb".
	Model string `json:"model,omitempty"`
}

// USBSpec declares USB capacity and boot-time devices.
type USBSpec struct {
	// Controllers is how many qemu-xhci controllers to attach. Zero
	// means one when Devices or CCID need USB.
	Controllers int `json:"controllers,omitempty"`
	// Devices are attached at boot. Hotplug adds more later.
	Devices []USBDeviceSpec `json:"devices,omitempty"`
}

// USBDeviceSpec is one USB device at create time.
type USBDeviceSpec struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // storage, tablet
	// Image is required for kind=storage.
	Image string `json:"image,omitempty"`
}

// CCIDSpec is one smart-card reader at create time.
type CCIDSpec struct {
	ID string `json:"id"`
}

// Validate reports a Spec the manager must refuse.
func (s Spec) Validate() error {
	var errs []error
	if err := validateID(s.ID); err != nil {
		errs = append(errs, err)
	}
	if s.MemoryMB <= 0 {
		errs = append(errs, errors.New("memoryMb must be positive"))
	}
	if s.MemoryMB > 512*1024 {
		errs = append(errs, errors.New("memoryMb is too large"))
	}
	if s.VCPUs <= 0 {
		errs = append(errs, errors.New("vcpus must be positive"))
	}
	if s.VCPUs > 256 {
		errs = append(errs, errors.New("vcpus is too large"))
	}

	fw := s.FirmwareNorm()
	if fw != FirmwareBIOS && fw != FirmwareUEFI {
		errs = append(errs, fmt.Errorf("firmware %q is not bios or uefi", s.Firmware))
	}

	disks, err := s.ResolvedDisks()
	if err != nil {
		errs = append(errs, err)
	} else {
		seen := map[string]struct{}{}
		for i, d := range disks {
			if d.Name == "" {
				errs = append(errs, fmt.Errorf("disks[%d].name is empty", i))
			}
			if _, ok := seen[d.Name]; ok {
				errs = append(errs, fmt.Errorf("disk name %q is repeated", d.Name))
			}
			seen[d.Name] = struct{}{}
			if d.Image == "" && d.SizeGB <= 0 {
				errs = append(errs, fmt.Errorf("disk %q needs sizeGb or image", d.Name))
			}
			if d.SizeGB > 10*1024 {
				errs = append(errs, fmt.Errorf("disk %q is too large", d.Name))
			}
			bus := d.BusNorm()
			if bus != "virtio" && bus != "scsi" && bus != "ide" {
				errs = append(errs, fmt.Errorf("disk %q bus %q is not virtio, scsi or ide", d.Name, d.Bus))
			}
		}
	}

	if s.TPM != nil {
		v := s.TPM.VersionNorm()
		if v != "2.0" && v != "1.2" {
			errs = append(errs, fmt.Errorf("tpm.version %q is not 1.2 or 2.0", s.TPM.Version))
		}
		m := s.TPM.ModelNorm()
		if m != "tpm-tis" && m != "tpm-crb" {
			errs = append(errs, fmt.Errorf("tpm.model %q is not tpm-tis or tpm-crb", s.TPM.Model))
		}
	}

	if s.USB != nil {
		if s.USB.Controllers < 0 || s.USB.Controllers > 4 {
			errs = append(errs, errors.New("usb.controllers must be between 0 and 4"))
		}
		seen := map[string]struct{}{}
		for i, d := range s.USB.Devices {
			if err := validateDeviceID(d.ID); err != nil {
				errs = append(errs, fmt.Errorf("usb.devices[%d].id: %w", i, err))
			}
			if _, ok := seen[d.ID]; ok {
				errs = append(errs, fmt.Errorf("usb device id %q is repeated", d.ID))
			}
			seen[d.ID] = struct{}{}
			switch d.Kind {
			case "storage":
				if d.Image == "" {
					errs = append(errs, fmt.Errorf("usb device %q needs image", d.ID))
				}
			case "tablet":
			default:
				errs = append(errs, fmt.Errorf("usb device %q kind %q is not storage or tablet", d.ID, d.Kind))
			}
		}
	}

	seenCCID := map[string]struct{}{}
	for i, c := range s.CCID {
		if err := validateDeviceID(c.ID); err != nil {
			errs = append(errs, fmt.Errorf("ccid[%d].id: %w", i, err))
		}
		if _, ok := seenCCID[c.ID]; ok {
			errs = append(errs, fmt.Errorf("ccid id %q is repeated", c.ID))
		}
		seenCCID[c.ID] = struct{}{}
	}

	return errors.Join(errs...)
}

// FirmwareNorm returns bios when Firmware is empty.
func (s Spec) FirmwareNorm() string {
	switch strings.ToLower(strings.TrimSpace(s.Firmware)) {
	case "", FirmwareBIOS:
		return FirmwareBIOS
	case FirmwareUEFI, "ovmf":
		return FirmwareUEFI
	default:
		return strings.ToLower(strings.TrimSpace(s.Firmware))
	}
}

// ResolvedDisks returns the disk list, expanding the DiskGB/Image
// shorthand when Disks is empty.
func (s Spec) ResolvedDisks() ([]DiskSpec, error) {
	if len(s.Disks) > 0 {
		out := make([]DiskSpec, len(s.Disks))
		copy(out, s.Disks)
		for i := range out {
			if out[i].Name == "" {
				if i == 0 {
					out[i].Name = "disk"
				} else {
					out[i].Name = fmt.Sprintf("disk%d", i)
				}
			}
		}
		return out, nil
	}
	if s.DiskGB <= 0 && s.Image == "" {
		return nil, errors.New("diskGb or disks is required")
	}
	return []DiskSpec{{
		Name:   "disk",
		SizeGB: s.DiskGB,
		Image:  s.Image,
		Bus:    "virtio",
	}}, nil
}

// BusNorm returns virtio when Bus is empty.
func (d DiskSpec) BusNorm() string {
	b := strings.ToLower(strings.TrimSpace(d.Bus))
	if b == "" {
		return "virtio"
	}
	return b
}

// VersionNorm returns 2.0 when Version is empty.
func (t TPMSpec) VersionNorm() string {
	v := strings.TrimSpace(t.Version)
	if v == "" {
		return "2.0"
	}
	return v
}

// ModelNorm returns tpm-tis when Model is empty.
func (t TPMSpec) ModelNorm() string {
	m := strings.ToLower(strings.TrimSpace(t.Model))
	if m == "" {
		return "tpm-tis"
	}
	return m
}

// USBControllers returns how many xhci controllers to attach.
func (s Spec) USBControllers() int {
	need := len(s.CCID) > 0
	n := 0
	if s.USB != nil {
		n = s.USB.Controllers
		if len(s.USB.Devices) > 0 {
			need = true
		}
	}
	if n > 0 {
		return n
	}
	if need {
		return 1
	}
	return 0
}

// DisplayName falls back to the id.
func (s Spec) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

// UnitName is the systemd instance unit this machine runs under.
func (s Spec) UnitName() string {
	return "qemu-vm@" + s.ID + ".service"
}

// PatchSpec is a partial update. Nil pointer fields mean "leave as is".
// Nested objects replace as a whole when the pointer is set.
type PatchSpec struct {
	Name     *string     `json:"name,omitempty"`
	Notes    *string     `json:"notes,omitempty"`
	MemoryMB *int64      `json:"memoryMb,omitempty"`
	VCPUs    *int        `json:"vcpus,omitempty"`
	Firmware *string     `json:"firmware,omitempty"`
	Disks    *[]DiskSpec `json:"disks,omitempty"`
	TPM      *TPMSpec    `json:"tpm,omitempty"`
	USB      *USBSpec    `json:"usb,omitempty"`
	CCID     *[]CCIDSpec `json:"ccid,omitempty"`
	// ClearTPM removes a TPM from the desired state.
	ClearTPM bool `json:"clearTpm,omitempty"`
	// ClearUSB removes USB from the desired state.
	ClearUSB bool `json:"clearUsb,omitempty"`
}

// Apply returns a copy of s with patch applied. Id and primary shorthand
// DiskGB/Image are not patchable; use Disks.
func (s Spec) Apply(p PatchSpec) Spec {
	out := s
	if p.Name != nil {
		out.Name = *p.Name
	}
	if p.Notes != nil {
		out.Notes = *p.Notes
	}
	if p.MemoryMB != nil {
		out.MemoryMB = *p.MemoryMB
	}
	if p.VCPUs != nil {
		out.VCPUs = *p.VCPUs
	}
	if p.Firmware != nil {
		out.Firmware = *p.Firmware
	}
	if p.Disks != nil {
		out.Disks = append([]DiskSpec(nil), (*p.Disks)...)
		out.DiskGB = 0
		out.Image = ""
	}
	if p.ClearTPM {
		out.TPM = nil
	} else if p.TPM != nil {
		cp := *p.TPM
		out.TPM = &cp
	}
	if p.ClearUSB {
		out.USB = nil
	} else if p.USB != nil {
		cp := *p.USB
		out.USB = &cp
	}
	if p.CCID != nil {
		out.CCID = append([]CCIDSpec(nil), (*p.CCID)...)
	}
	return out
}

func validateID(id string) error {
	if id == "" {
		return errors.New("id is required")
	}
	for i, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-'
		if i == 0 && (r == '.' || r == '-' || r == '_') {
			return fmt.Errorf("id %q must start with a letter or digit", id)
		}
		if !ok {
			return fmt.Errorf("id %q has a character inventory cannot carry", id)
		}
	}
	return nil
}

func validateDeviceID(id string) error {
	if id == "" {
		return errors.New("id is required")
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '-'
		if !ok {
			return fmt.Errorf("%q has a character QEMU cannot use as a device id", id)
		}
	}
	return nil
}
