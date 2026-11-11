package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/lifecycle"
	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
)

const createBodyLimit = 64 << 10
const patchBodyLimit = 64 << 10

func (s *Server) handleCreateMachine(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, createBodyLimit)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: %s", err.Error())
		return
	}
	spec := req.Spec

	// A create that carries the cluster token comes from the node that
	// did the placement, so this node is the place. A node with no peer
	// and no named node is the place also.
	placed := r.Header.Get(peer.ClusterTokenHeader) != ""
	if !placed && (req.Node != "" || s.peers.Len() > 0) {
		if err := spec.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		target, status, problem := s.place(r.Context(), req)
		if problem != "" {
			writeError(w, status, "%s", problem)
			return
		}
		if target != nil {
			s.createOnPeer(w, r, *target, spec)
			return
		}
	}

	if s.life == nil || !s.life.Enabled() {
		writeError(w, http.StatusNotImplemented, "machine lifecycle is disabled on this labview")
		return
	}
	user := s.identity(r)

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

func (s *Server) handlePatchMachine(w http.ResponseWriter, r *http.Request) {
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
			"machine %q was not created by the lifecycle API, so labview will not patch it", m.ID)
		return
	}
	if !s.leases.CanWrite(m.ID, user) {
		s.writeLeaseDenied(w, m.ID, user, "patch without the lease")
		return
	}

	var patch lifecycle.PatchSpec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, patchBodyLimit)).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: %s", err.Error())
		return
	}

	result, err := s.life.Patch(r.Context(), m.ID, patch)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, lifecycle.ErrDisabled):
			status = http.StatusNotImplemented
		case errors.Is(err, lifecycle.ErrNotManaged):
			status = http.StatusConflict
		}
		writeError(w, status, "%s", err.Error())
		return
	}

	s.activity.Record(activity.Event{
		Machine: m.ID, User: user, Action: "patched",
		Detail: fmtNeedsRestart(result.NeedsRestart),
	})
	writeJSON(w, http.StatusOK, result)
}

func fmtNeedsRestart(v bool) string {
	if v {
		return "needsRestart"
	}
	return "live"
}
