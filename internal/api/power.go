package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/host"
)

// powerRequest names an operation and nothing else.
//
// A client never names a unit: it names a machine id, and labview maps that
// to a unit through the inventory, which is therefore also the allowlist
// (design section 12).
type powerRequest struct {
	Op string `json:"op"` // start, stop, restart
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	user := s.identity(r)

	var req powerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: %s", err.Error())
		return
	}

	op := host.Op(req.Op)
	if !op.Valid() {
		writeError(w, http.StatusBadRequest,
			"unknown operation %q; expected start, stop or restart", req.Op)
		return
	}
	if !m.CanPower() {
		writeError(w, http.StatusNotImplemented,
			"machine %q has no systemd unit in the inventory, so labview will not manage it", m.ID)
		return
	}

	// Power operations require the write lease, the same one that gates the
	// keyboard: restarting a machine somebody else is driving should be as
	// impossible as typing into it (design section 12).
	//
	// CanWrite rather than Touch: pressing restart is an exercise of
	// control, but it should not silently prolong it.
	if !s.leases.CanWrite(m.ID, user) {
		current, held := s.leases.Get(m.ID)
		body := errorBody{Error: "you do not hold the write lease for this machine"}
		if held {
			body.Holder = current.Holder
			body.Expires = current.Expires.Format(time.RFC3339)
		} else {
			body.Error = "take control of this machine before changing its power state"
		}
		s.activity.Record(activity.Event{
			Machine: m.ID, User: user, Action: activity.ActionDenied,
			Detail: "power " + req.Op + " without the lease",
		})
		writeJSON(w, http.StatusConflict, body)
		return
	}

	if err := s.hosts.Power(r.Context(), m, op); err != nil {
		status := http.StatusBadGateway
		if noUnit(err) || hostUnavailable(err) {
			status = http.StatusNotImplemented
		}
		s.log.Error("power operation failed",
			"machine", m.ID, "op", req.Op, "user", user, "error", err)
		writeError(w, status, "%s", err.Error())
		return
	}

	// Restart is the only destructive thing in an otherwise read-mostly
	// tool, so it is logged with the identity from the proxy (design
	// section 12). The UI confirms before getting here.
	s.activity.Record(activity.Event{
		Machine: m.ID, User: user, Action: activity.ActionPower, Detail: req.Op,
	})

	u, err := s.hosts.UnitState(r.Context(), m)
	resp := map[string]any{"ok": true, "op": req.Op}
	if err == nil {
		resp["unit"] = u
	}
	writeJSON(w, http.StatusOK, resp)
}
