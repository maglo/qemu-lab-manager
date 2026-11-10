package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/qmp"
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

	if err := s.control.Hotplug(r.Context(), target(m), qmp.HotplugSpec{
		ID: req.ID, Kind: req.Kind, Image: req.Image, Reader: req.Reader,
	}); err != nil {
		if errors.Is(err, qmp.ErrBadHotplug) {
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
