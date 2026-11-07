package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/lifecycle"
)

const createBodyLimit = 64 << 10

func (s *Server) handleCreateMachine(w http.ResponseWriter, r *http.Request) {
	if s.life == nil || !s.life.Enabled() {
		writeError(w, http.StatusNotImplemented, "machine lifecycle is disabled on this labview")
		return
	}
	user := s.identity(r)

	var spec lifecycle.Spec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, createBodyLimit)).Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: %s", err.Error())
		return
	}

	mach, err := s.life.Create(r.Context(), spec)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, lifecycle.ErrDisabled):
			status = http.StatusNotImplemented
		case errors.Is(err, lifecycle.ErrExists):
			status = http.StatusConflict
		}
		writeError(w, status, "%s", err.Error())
		return
	}

	// The creator holds the first lease so nobody else can destroy or
	// drive the machine before they are ready (cluster.md section 4).
	if _, err := s.leases.Acquire(mach.ID, user); err != nil {
		s.log.Warn("create could not grant the first lease",
			"machine", mach.ID, "user", user, "error", err)
	}

	s.activity.Record(activity.Event{
		Machine: mach.ID, User: user, Action: activity.ActionCreated,
	})

	sum := s.summarise(r.Context(), mach, s.brokers.States(), s.leases.All())
	writeJSON(w, http.StatusCreated, sum)
}

func (s *Server) handleDestroyMachine(w http.ResponseWriter, r *http.Request) {
	if s.life == nil || !s.life.Enabled() {
		writeError(w, http.StatusNotImplemented, "machine lifecycle is disabled on this labview")
		return
	}
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	user := s.identity(r)

	if !s.life.Managed(m.ID) {
		writeError(w, http.StatusConflict,
			"machine %q was not created by the lifecycle API, so labview will not destroy it", m.ID)
		return
	}

	if !s.leases.CanWrite(m.ID, user) {
		current, held := s.leases.Get(m.ID)
		body := errorBody{Error: "you do not hold the write lease for this machine"}
		if held {
			body.Holder = current.Holder
			body.Expires = current.Expires.Format(time.RFC3339)
		} else {
			body.Error = "take control of this machine before destroying it"
		}
		s.activity.Record(activity.Event{
			Machine: m.ID, User: user, Action: activity.ActionDenied,
			Detail: "destroy without the lease",
		})
		writeJSON(w, http.StatusConflict, body)
		return
	}

	if err := s.life.Destroy(r.Context(), m.ID); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, lifecycle.ErrDisabled) {
			status = http.StatusNotImplemented
		}
		if errors.Is(err, lifecycle.ErrNotManaged) {
			status = http.StatusConflict
		}
		writeError(w, status, "%s", err.Error())
		return
	}

	_ = s.leases.Release(m.ID, user)
	s.activity.Record(activity.Event{
		Machine: m.ID, User: user, Action: activity.ActionDestroyed,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": m.ID})
}
