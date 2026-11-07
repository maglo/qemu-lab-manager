package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/telemetry"
)

// handleTelemetry serves the event buffer as NDJSON.
//
// The default follows live events after the ring. ?follow=0 dumps the
// ring and closes.
func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	if s.telemetry == nil {
		writeError(w, http.StatusNotFound, "telemetry is not enabled")
		return
	}

	follow := true
	if raw := r.URL.Query().Get("follow"); raw != "" {
		follow = boolParam(r, "follow")
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	enc := json.NewEncoder(w)

	for _, ev := range s.telemetry.Snapshot() {
		if err := enc.Encode(ev); err != nil {
			return
		}
	}
	if canFlush {
		flusher.Flush()
	}
	if !follow {
		return
	}

	ch := s.telemetry.Subscribe(64)
	defer s.telemetry.Unsubscribe(ch)

	// Heartbeat so a proxy does not close an idle stream. Empty lines are
	// not JSON objects; a consumer that reads line-by-line skips them.
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
	}
}

// peerFailTracker emits peer-fail telemetry once per distinct error text,
// so the machines listing poll does not flood the stream.
type peerFailTracker struct {
	mu   sync.Mutex
	last map[string]string
	bus  *telemetry.Bus
}

func newPeerFailTracker(bus *telemetry.Bus) *peerFailTracker {
	return &peerFailTracker{last: make(map[string]string), bus: bus}
}

func (t *peerFailTracker) note(peerName, errText string) {
	if t == nil || t.bus == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last[peerName] == errText {
		return
	}
	t.last[peerName] = errText
	t.bus.PeerFail(peerName, errText)
}

func (t *peerFailTracker) clear(peerName string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.last, peerName)
	t.mu.Unlock()
}
