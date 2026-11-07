package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
)

// deviceRequest is POST /api/machines/{id}/devices.
//
// Kind names the LCM hotplug verb. Scenario steps will call this path;
// they do not invent QEMU flags (docs/design/scenarios.md).
type deviceRequest struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"` // usb-storage, usb-tablet, usb-ccid, ccid-card
	Image string `json:"image,omitempty"`
	// Reader is the usb-ccid device id for kind=ccid-card.
	Reader string `json:"reader,omitempty"`
}

func (s *Server) handleAddDevice(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	user := s.identity(r)

	var req deviceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: %s", err.Error())
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !m.HasControl() {
		writeError(w, http.StatusNotImplemented,
			"machine %q has no control socket, so labview cannot hotplug devices", m.ID)
		return
	}
	if !s.leases.CanWrite(m.ID, user) {
		s.writeLeaseDenied(w, m.ID, user, "hotplug without the lease")
		return
	}

	if err := s.hotplugDevice(r, m.ID, req); err != nil {
		if isBadHotplug(err) {
			writeError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		s.writeControlError(w, m.ID, "device_add", err)
		return
	}
	s.activity.Record(activity.Event{
		Machine: m.ID, User: user, Action: "device-add",
		Detail: req.Kind + " " + req.ID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": req.ID, "kind": req.Kind})
}

func (s *Server) handleRemoveDevice(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	user := s.identity(r)
	devID := r.PathValue("deviceId")
	if devID == "" {
		writeError(w, http.StatusBadRequest, "device id is required")
		return
	}
	if !m.HasControl() {
		writeError(w, http.StatusNotImplemented,
			"machine %q has no control socket, so labview cannot remove devices", m.ID)
		return
	}
	if !s.leases.CanWrite(m.ID, user) {
		s.writeLeaseDenied(w, m.ID, user, "device remove without the lease")
		return
	}
	if err := s.control.DeviceDel(r.Context(), target(m), devID); err != nil {
		s.writeControlError(w, m.ID, "device_del", err)
		return
	}
	s.activity.Record(activity.Event{
		Machine: m.ID, User: user, Action: "device-del", Detail: devID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": devID})
}

func (s *Server) hotplugDevice(r *http.Request, machineID string, req deviceRequest) error {
	m, ok := s.inventory.Current().Get(machineID)
	if !ok {
		return badHotplug("no machine")
	}
	tgt := target(m)
	switch req.Kind {
	case "usb-tablet":
		return s.control.DeviceAdd(r.Context(), tgt, "usb-tablet", map[string]any{"id": req.ID})
	case "usb-storage":
		if req.Image == "" {
			return badHotplug("usb-storage needs image")
		}
		return s.control.HotplugUSBStorage(r.Context(), tgt, req.ID, req.Image)
	case "usb-ccid":
		return s.control.DeviceAdd(r.Context(), tgt, "usb-ccid", map[string]any{"id": req.ID})
	case "ccid-card":
		if req.Reader == "" {
			return badHotplug("ccid-card needs reader")
		}
		return s.control.DeviceAdd(r.Context(), tgt, "ccid-card-emulated", map[string]any{
			"id":  req.ID,
			"bus": req.Reader + ".0",
		})
	default:
		return badHotplug(fmt.Sprintf("kind %q is not usb-tablet, usb-storage, usb-ccid or ccid-card", req.Kind))
	}
}

type badHotplugError string

func (e badHotplugError) Error() string { return string(e) }

func badHotplug(msg string) error { return badHotplugError(msg) }

func isBadHotplug(err error) bool {
	_, ok := err.(badHotplugError)
	return ok
}

func (s *Server) writeLeaseDenied(w http.ResponseWriter, machine, user, detail string) {
	current, held := s.leases.Get(machine)
	body := errorBody{Error: "you do not hold the write lease for this machine"}
	if held {
		body.Holder = current.Holder
		body.Expires = current.Expires.Format(time.RFC3339)
	} else {
		body.Error = "take control of this machine first"
	}
	s.activity.Record(activity.Event{
		Machine: machine, User: user, Action: activity.ActionDenied, Detail: detail,
	})
	writeJSON(w, http.StatusConflict, body)
}
