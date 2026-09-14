package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
	"github.com/maglo/qemu-lab-manager/labview/internal/qmp"
)

// defaultHoldMs is how long QEMU holds a chord down when a client says
// nothing. QEMU's own default is short enough that a guest can miss a chord
// it has to see, such as Ctrl+Alt+F3 at a console.
const defaultHoldMs = 100

// maxHoldMs bounds the hold, because QEMU blocks its own command loop for the
// whole of it.
const maxHoldMs = 1000

// keysRequest is the body of POST /api/machines/{id}/keys.
//
// One call is one chord: QEMU presses every key together and releases them
// together. A client that wants two chords makes two calls.
type keysRequest struct {
	Keys   []string `json:"keys"`
	HoldMs int      `json:"holdMs"`
}

// target maps an inventory entry to the control socket it names. A client
// names a machine id and nothing else, exactly as it does for power.
func target(m inventory.Machine) qmp.Target {
	return qmp.Target{ID: m.ID, Network: m.ControlNetwork(), Address: m.Control}
}

// handleKeys sends one chord to a machine.
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	user := s.identity(r)

	var req keysRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: %s", err.Error())
		return
	}
	if err := qmp.ValidateKeys(req.Keys); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if !m.HasControl() {
		writeError(w, http.StatusNotImplemented,
			"machine %q has no control socket in the inventory, so labview cannot send keys to it", m.ID)
		return
	}

	// The keyboard is the keyboard whichever way it is reached, so this is
	// the lease that gates the framebuffer and the serial line (design
	// section 4). Touch rather than CanWrite: a chord is input, and input
	// pushes the expiry out.
	if !s.leases.Touch(m.ID, user) {
		current, held := s.leases.Get(m.ID)
		body := errorBody{Error: "take control of this machine before sending keys to it"}
		if held {
			body.Error = "you do not hold the write lease for this machine"
			body.Holder = current.Holder
			body.Expires = current.Expires.Format(time.RFC3339)
		}
		s.activity.Record(activity.Event{
			Machine: m.ID, User: user, Action: activity.ActionDenied,
			Detail: "keys without the lease",
		})
		writeJSON(w, http.StatusConflict, body)
		return
	}

	holdMs := req.HoldMs
	if holdMs <= 0 {
		holdMs = defaultHoldMs
	}
	if holdMs > maxHoldMs {
		holdMs = maxHoldMs
	}

	if err := s.control.SendKeys(r.Context(), target(m), req.Keys, holdMs); err != nil {
		s.writeControlError(w, m.ID, "send-key", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": req.Keys, "holdMs": holdMs})
}

// handleScreenshot captures one frame of a machine's screen.
//
// It needs no lease. The wall is view-only for everyone and shows every
// machine's screen already (design sections 4 and 10), so a capture of that
// same screen is a read, and read is free. A screenshot tile on a wall of
// forty machines is the case that decides it: nobody holds forty leases.
func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	if !m.HasControl() {
		writeError(w, http.StatusNotImplemented,
			"machine %q has no control socket in the inventory, so labview cannot capture its screen", m.ID)
		return
	}

	png, err := s.control.Screenshot(r.Context(), target(m))
	if errors.Is(err, qmp.ErrNoCaptureDir) {
		writeError(w, http.StatusNotImplemented, "screen capture is disabled on this labview")
		return
	}
	if err != nil {
		s.writeControlError(w, m.ID, "screendump", err)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

// writeControlError answers a failed exchange without quoting the socket.
//
// A dial failure names the endpoint it failed on, and section 6 keeps every
// address off every client, so the text stays in labview's log and the client
// gets the fact of the failure. QEMU's own refusal is different: it names the
// command and the parameter, so a harness gets to read it.
func (s *Server) writeControlError(w http.ResponseWriter, id, command string, err error) {
	var refused *qmp.CommandError
	if errors.As(err, &refused) {
		writeError(w, http.StatusBadGateway, "the machine refused %s: %s", command, refused.Error())
		return
	}
	s.log.Error("control command failed", "machine", id, "command", command, "error", err)
	writeError(w, http.StatusBadGateway, "the control socket of machine %q did not answer", id)
}
