package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/coder/websocket"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
	"github.com/maglo/qemu-lab-manager/labview/internal/rfb"
)

// handleConsoleWS proxies binary RFB between a browser and a machine's VNC
// port (design section 5).
//
// This is the easy channel: QEMU shares its VNC port across clients, so
// labview holds no state -- one browser websocket, one TCP dial, copy bytes
// both ways (design section 2).
//
// Two rules shape the details. The stream stays pure binary and nothing is
// injected into it, because RFB has its own handshake (design section 11): a
// console websocket therefore carries no status frames at all, and anything
// labview needs to say about control is said on the serial channel or over the
// API. And input is gated on the write lease, the same lease that gates the
// keyboard elsewhere -- enforced here rather than only by flipping noVNC's
// viewOnly, so a second tab or a scripted client cannot type either.
func (s *Server) handleConsoleWS(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	user := s.identity(r)

	if !m.HasConsole() {
		writeError(w, http.StatusNotImplemented, "machine %q has no console in the inventory", m.ID)
		return
	}

	wantWrite := boolParam(r, "write")

	// Dial before upgrading, so a machine that is not listening produces an
	// HTTP error a fetch can read rather than an immediate socket close.
	dctx, dcancel := context.WithTimeout(r.Context(), s.cfg.DialTimeout)
	defer dcancel()
	var dialer net.Dialer
	upstream, err := dialer.DialContext(dctx, "tcp", m.VNC)
	if err != nil {
		s.log.Debug("vnc dial failed", "machine", m.ID, "error", err)
		writeError(w, http.StatusServiceUnavailable,
			"machine %q is not accepting console connections", m.ID)
		return
	}
	defer upstream.Close()

	c, err := acceptWS(w, r)
	if err != nil {
		s.log.Debug("console websocket upgrade failed", "machine", m.ID, "error", err)
		return
	}
	c.SetReadLimit(wsReadLimit)
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var leaseToken uint64
	if wantWrite {
		if _, token, err := s.leases.AcquireConn(m.ID, user); err == nil {
			leaseToken = token
			s.activity.Record(activity.Event{
				Machine: m.ID, User: user, Action: activity.ActionGranted, Channel: "console",
			})
		} else if errors.Is(err, lease.ErrHeld) {
			// Still connect, read-only. The UI already knows who holds it
			// from the machines listing.
			s.activity.Record(activity.Event{
				Machine: m.ID, User: user, Action: activity.ActionDenied,
				Channel: "console", Detail: "control held elsewhere",
			})
		}
	}
	defer func() {
		if leaseToken != 0 {
			s.leases.ReleaseConn(m.ID, user, leaseToken)
			s.activity.Record(activity.Event{
				Machine: m.ID, User: user, Action: activity.ActionReleased,
				Channel: "console", Detail: "socket closed",
			})
		}
	}()

	s.activity.Record(activity.Event{
		Machine: m.ID, User: user, Action: activity.ActionAttach, Channel: "console",
	})
	defer s.activity.Record(activity.Event{
		Machine: m.ID, User: user, Action: activity.ActionDetach, Channel: "console",
	})

	done := make(chan struct{}, 2)

	// Machine to browser: straight through, untouched.
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32<<10)
		for {
			n, err := upstream.Read(buf)
			if n > 0 {
				if werr := writeOutput(ctx, c, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					s.log.Debug("vnc read ended", "machine", m.ID, "error", err)
				}
				return
			}
		}
	}()

	// Browser to machine: parsed for message boundaries, then forwarded
	// according to the lease.
	go func() {
		defer func() { done <- struct{}{} }()
		s.consoleReadLoop(ctx, c, upstream, m.ID, user)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// consoleReadLoop forwards client RFB messages upstream, dropping input when
// the client does not hold the lease.
func (s *Server) consoleReadLoop(ctx context.Context, c *websocket.Conn, upstream net.Conn, machineID, user string) {
	filter := rfb.NewInputFilter()
	var loggedUnframed, loggedDrop bool

	for {
		kind, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageBinary {
			// RFB is binary only. A text frame is not part of the protocol,
			// so it is not forwarded into it.
			continue
		}

		// Every byte goes through the filter even while the lease is held,
		// because the parser is stateful: forwarding unparsed bytes would
		// leave it unable to find the next message boundary if the lease
		// then lapsed mid-session.
		msgs, ferr := filter.Split(data)
		for _, msg := range msgs {
			if msg.IsInput && !s.leases.Touch(machineID, user) {
				if !loggedDrop {
					loggedDrop = true
					s.log.Debug("dropping console input from a client with no lease",
						"machine", machineID, "user", user)
				}
				continue
			}
			if _, werr := upstream.Write(msg.Bytes); werr != nil {
				return
			}
		}

		if ferr != nil {
			// Framing is lost, so input can no longer be told from viewing
			// traffic. Forwarding blindly would defeat the lease and
			// guessing would corrupt the session, so stop reading from this
			// client. The framebuffer keeps updating until it closes.
			if !loggedUnframed {
				loggedUnframed = true
				s.log.Warn("console client sent an RFB message labview does not know; input disabled for this session",
					"machine", machineID, "user", user, "error", ferr)
			}
			return
		}
	}
}
