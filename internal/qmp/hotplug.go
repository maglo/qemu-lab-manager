package qmp

import (
	"context"
	"errors"
	"fmt"
)

// ErrBadHotplug is a client error for a hotplug request the server refuses.
var ErrBadHotplug = errors.New("bad hotplug request")

// HotplugSpec is one live device attach (docs/design/cluster.md section 4).
// Kind matches POST /api/machines/{id}/devices.
type HotplugSpec struct {
	ID     string
	Kind   string // usb-tablet, usb-storage, usb-ccid, ccid-card
	Image  string // usb-storage
	Reader string // usb-ccid id for ccid-card
}

// Hotplug attaches a device. Scenario steps and the HTTP API share this.
func (m *Manager) Hotplug(ctx context.Context, t Target, spec HotplugSpec) error {
	if spec.ID == "" {
		return fmt.Errorf("%w: id is required", ErrBadHotplug)
	}
	switch spec.Kind {
	case "usb-tablet":
		return m.DeviceAdd(ctx, t, "usb-tablet", map[string]any{"id": spec.ID})
	case "usb-storage":
		if spec.Image == "" {
			return fmt.Errorf("%w: usb-storage needs image", ErrBadHotplug)
		}
		return m.HotplugUSBStorage(ctx, t, spec.ID, spec.Image)
	case "usb-ccid":
		return m.DeviceAdd(ctx, t, "usb-ccid", map[string]any{"id": spec.ID})
	case "ccid-card":
		if spec.Reader == "" {
			return fmt.Errorf("%w: ccid-card needs reader", ErrBadHotplug)
		}
		return m.DeviceAdd(ctx, t, "ccid-card-emulated", map[string]any{
			"id":  spec.ID,
			"bus": spec.Reader + ".0",
		})
	default:
		return fmt.Errorf("%w: kind %q is not usb-tablet, usb-storage, usb-ccid or ccid-card", ErrBadHotplug, spec.Kind)
	}
}

// ValidHotplugKind reports whether kind is one Hotplug accepts.
func ValidHotplugKind(kind string) bool {
	switch kind {
	case "usb-tablet", "usb-storage", "usb-ccid", "ccid-card":
		return true
	}
	return false
}
