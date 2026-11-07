// Package lifecycle creates and destroys machines on one node
// (docs/design/cluster.md section 4).
//
// The API is the source of machine truth. A create writes the disk, the
// QEMU argv file and an inventory entry. A destroy stops the unit and
// removes those files. Power still goes through systemd.
package lifecycle

import (
	"errors"
	"fmt"
)

// Spec is the create document for one machine.
type Spec struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Notes    string `json:"notes,omitempty"`
	MemoryMB int64  `json:"memoryMb"`
	VCPUs    int    `json:"vcpus"`
	DiskGB   int64  `json:"diskGb"`
	// Image is a backing file path. Empty means a blank disk.
	Image string `json:"image,omitempty"`
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
	if s.DiskGB <= 0 {
		errs = append(errs, errors.New("diskGb must be positive"))
	}
	if s.DiskGB > 10*1024 {
		errs = append(errs, errors.New("diskGb is too large"))
	}
	return errors.Join(errs...)
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

func validateID(id string) error {
	if id == "" {
		return errors.New("id is required")
	}
	// Same shape as inventory ids: path-safe, one path segment.
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
