package api

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
)

// handleLogsWS tails a machine's journal.
//
// A VM that failed to start has its reason here and nowhere else, which is the
// case where a developer currently has to SSH to the hypervisor (design
// section 8).
//
// Journal lines are structured, so this channel is text frames throughout --
// unlike the serial channel, there is no raw byte stream to keep them out of.
func (s *Server) handleLogsWS(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	if !m.CanPower() {
		writeError(w, http.StatusNotImplemented,
			"machine %q has no systemd unit in the inventory, so it has no journal to tail", m.ID)
		return
	}

	c, err := acceptWS(w, r)
	if err != nil {
		return
	}
	c.SetReadLimit(wsReadLimit)
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	lines, err := s.hosts.TailLogs(ctx, m)
	if err != nil {
		writeStatus(ctx, c, map[string]string{
			"type":    "error",
			"machine": m.ID,
			"message": err.Error(),
		})
		c.Close(websocket.StatusNormalClosure, "journal unavailable")
		return
	}

	// A client that goes away should stop journalctl, so notice its close.
	go func() {
		defer cancel()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				c.Close(websocket.StatusNormalClosure, "journal stream ended")
				return
			}
			if err := writeStatus(ctx, c, line); err != nil {
				return
			}
		}
	}
}
