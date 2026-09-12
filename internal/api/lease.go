package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
)

// leaseRequest is the body of POST /api/machines/{id}/lease.
//
// One endpoint for acquire, renew and release (design section 5), because a
// client that has the lease and a client that wants it are the same client a
// moment apart.
type leaseRequest struct {
	Action string `json:"action"` // acquire, renew, release
}

type leaseResponse struct {
	Held   bool         `json:"held"`
	Mine   bool         `json:"mine"`
	Lease  *lease.Lease `json:"lease,omitempty"`
	Reason string       `json:"reason,omitempty"`
}

func (s *Server) handleLease(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	user := s.identity(r)

	var req leaseRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "body is not valid JSON: %s", err.Error())
			return
		}
	}
	if req.Action == "" {
		req.Action = "acquire"
	}

	switch req.Action {
	case "acquire":
		l, err := s.leases.Acquire(m.ID, user)
		if errors.Is(err, lease.ErrHeld) {
			// Refused, but say who has it: the UI shows the holder and
			// offers to watch instead (design section 4).
			s.activity.Record(activity.Event{
				Machine: m.ID, User: user, Action: activity.ActionDenied,
				Detail: "control held by " + l.Holder,
			})
			writeJSON(w, http.StatusConflict, errorBody{
				Error:   "another user is driving this machine",
				Holder:  l.Holder,
				Expires: l.Expires.Format(time.RFC3339),
			})
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		s.activity.Record(activity.Event{
			Machine: m.ID, User: user, Action: activity.ActionGranted, Detail: "via API",
		})
		writeJSON(w, http.StatusOK, leaseResponse{Held: true, Mine: true, Lease: &l})

	case "renew":
		l, err := s.leases.Renew(m.ID, user)
		switch {
		case errors.Is(err, lease.ErrNoHolder):
			writeError(w, http.StatusConflict, "no lease to renew")
		case errors.Is(err, lease.ErrNotHolder):
			writeJSON(w, http.StatusConflict, errorBody{
				Error:   "you do not hold this lease",
				Holder:  l.Holder,
				Expires: l.Expires.Format(time.RFC3339),
			})
		case err != nil:
			writeError(w, http.StatusBadRequest, "%s", err.Error())
		default:
			writeJSON(w, http.StatusOK, leaseResponse{Held: true, Mine: true, Lease: &l})
		}

	case "release":
		err := s.leases.Release(m.ID, user)
		switch {
		case errors.Is(err, lease.ErrNoHolder):
			// Releasing a lease nobody holds is what the caller wanted.
			writeJSON(w, http.StatusOK, leaseResponse{Held: false})
		case errors.Is(err, lease.ErrNotHolder):
			writeError(w, http.StatusConflict, "you do not hold this lease")
		case err != nil:
			writeError(w, http.StatusBadRequest, "%s", err.Error())
		default:
			s.activity.Record(activity.Event{
				Machine: m.ID, User: user, Action: activity.ActionReleased, Detail: "via API",
			})
			writeJSON(w, http.StatusOK, leaseResponse{Held: false})
		}

	default:
		writeError(w, http.StatusBadRequest,
			"unknown action %q; expected acquire, renew or release", req.Action)
	}
}
