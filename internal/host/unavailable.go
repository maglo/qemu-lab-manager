package host

import (
	"context"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

// Unavailable is host access for a labview that is not running on the
// hypervisor.
//
// The framebuffer and the serial line are dialable over the network, so the
// wall and both consoles work exactly as they do on the hypervisor. The
// command line, the unit state and power operations are local, and this says
// so rather than guessing (design section 13).
type Unavailable struct{}

// NewUnavailable returns host access that reports every local-only facility
// as unavailable.
func NewUnavailable() *Unavailable { return &Unavailable{} }

func (*Unavailable) why(what string) error {
	return &ErrUnsupported{
		What: what,
		Why:  "labview is not running on this machine's hypervisor, so it cannot read the host",
	}
}

// Inspect implements Access. It returns the warning rather than an error, so
// the details tab explains itself instead of looking broken.
func (u *Unavailable) Inspect(_ context.Context, _ inventory.Machine) (Details, error) {
	return Details{Warnings: []string{u.why("host introspection").Error()}}, nil
}

// UnitState implements Access.
func (u *Unavailable) UnitState(_ context.Context, _ inventory.Machine) (Unit, error) {
	return Unit{}, u.why("unit state")
}

// Power implements Access.
func (u *Unavailable) Power(_ context.Context, _ inventory.Machine, _ Op) error {
	return u.why("power operations")
}

// Close implements Access.
func (*Unavailable) Close() error { return nil }

// Unavailable implements Access.
var _ Access = (*Unavailable)(nil)
