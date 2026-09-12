package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
	"github.com/maglo/qemu-lab-manager/labview/internal/serial"
)

// handleSerialWS streams a machine's serial line: scrollback first, then live
// bytes (design section 5).
//
// Opening this connects the client to the broker, not to the VM. The broker is
// already connected to the VM's serial port and stays that way, so a client
// can open a machine that is switched off, wait there, start it, and watch it
// come up from the first line (design section 11).
func (s *Server) handleSerialWS(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	user := s.identity(r)

	if !m.HasSerial() {
		writeError(w, http.StatusNotImplemented, "machine %q has no serial line in the inventory", m.ID)
		return
	}
	broker, ok := s.brokers.Get(m.ID)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no serial broker is running for %q", m.ID)
		return
	}

	// ?write=1 requests the lease at attach time, which is what a harness
	// wants. Without it, attach is read-only (design section 5).
	wantWrite := boolParam(r, "write")

	c, err := acceptWS(w, r)
	if err != nil {
		s.log.Debug("serial websocket upgrade failed", "machine", m.ID, "error", err)
		return
	}
	c.SetReadLimit(wsReadLimit)
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sub, scrollback, err := broker.Attach()
	if err != nil {
		writeStatus(ctx, c, serial.StatusMessage{
			Type: serial.StatusDown, Machine: m.ID,
			Message: "serial broker is not available: " + err.Error(), At: time.Now(),
		})
		c.Close(websocket.StatusTryAgainLater, "broker unavailable")
		return
	}
	defer sub.Close()

	var leaseToken uint64
	if wantWrite {
		l, token, err := s.leases.AcquireConn(m.ID, user)
		if errors.Is(err, lease.ErrHeld) {
			// Attach anyway, read-only, and say who is driving. Refusing
			// the connection would stop a developer watching a machine
			// somebody else has, which is explicitly allowed.
			writeStatus(ctx, c, controlStatus(m.ID, l, false,
				"another user is driving this machine"))
			s.activity.Record(activity.Event{
				Machine: m.ID, User: user, Action: activity.ActionDenied,
				Channel: "serial", Detail: "control held by " + l.Holder,
			})
		} else if err == nil {
			leaseToken = token
			writeStatus(ctx, c, controlStatus(m.ID, l, true, "you have control"))
			s.activity.Record(activity.Event{
				Machine: m.ID, User: user, Action: activity.ActionGranted, Channel: "serial",
			})
		}
	}
	defer func() {
		if leaseToken != 0 {
			s.leases.ReleaseConn(m.ID, user, leaseToken)
			s.activity.Record(activity.Event{
				Machine: m.ID, User: user, Action: activity.ActionReleased,
				Channel: "serial", Detail: "socket closed",
			})
		}
	}()

	s.activity.Record(activity.Event{
		Machine: m.ID, User: user, Action: activity.ActionAttach, Channel: "serial",
	})
	defer s.activity.Record(activity.Event{
		Machine: m.ID, User: user, Action: activity.ActionDetach, Channel: "serial",
	})

	// Scrollback goes out before any live byte, as one binary frame. The
	// broker guaranteed it does not overlap what follows.
	if len(scrollback) > 0 {
		if err := writeOutput(ctx, c, scrollback); err != nil {
			return
		}
	}

	leaseEvents, stopLease := s.leases.Subscribe()
	defer stopLease()

	// Reader: client frames are input. Binary frames are keystrokes for the
	// machine; text frames are reserved and ignored, so a client cannot
	// smuggle anything into the byte stream.
	go s.serialReadLoop(ctx, cancel, c, broker, m.ID, user)

	// Writer: broker events and lease transitions, in order.
	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-sub.Events():
			if !ok {
				// The broker dropped us: either shutting down, or this
				// subscriber fell too far behind.
				reason := "serial stream ended"
				if sub.Lagged() {
					reason = "client fell behind; reattach for fresh scrollback"
				}
				c.Close(websocket.StatusGoingAway, reason)
				return
			}
			if err := s.writeSerialEvent(ctx, c, ev); err != nil {
				return
			}

		case ev, ok := <-leaseEvents:
			if !ok {
				return
			}
			if ev.Machine != m.ID {
				continue
			}
			mine := ev.Lease.HeldBy(user)
			if err := writeStatus(ctx, c, controlStatus(m.ID, ev.Lease, mine, leaseMessage(ev, mine))); err != nil {
				return
			}
		}
	}
}

func (s *Server) writeSerialEvent(ctx context.Context, c *websocket.Conn, ev serial.Event) error {
	switch ev.Kind {
	case serial.EventOutput:
		return writeOutput(ctx, c, ev.Data)
	case serial.EventStatus:
		return writeStatus(ctx, c, ev.Status)
	}
	return nil
}

// serialReadLoop forwards client input to the machine, gated by the lease.
func (s *Server) serialReadLoop(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, broker *serial.Broker, machineID, user string) {
	defer cancel()
	for {
		kind, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageBinary {
			// Text from a client is not input. Ignoring it keeps the one
			// rule that matters: nothing a client says can enter the
			// machine's byte stream except as binary, deliberately.
			continue
		}
		if len(data) == 0 {
			continue
		}

		// Touch is the write gate and the keep-alive at once: every
		// keystroke pushes the idle deadline out, and a client with no
		// lease simply has its input dropped.
		if !s.leases.Touch(machineID, user) {
			l, held := s.leases.Get(machineID)
			msg := "take control of this machine before typing"
			if held {
				msg = "another user is driving this machine"
			}
			writeStatus(ctx, c, controlStatus(machineID, l, false, msg))
			continue
		}
		if err := broker.Write(data); err != nil {
			writeStatus(ctx, c, serial.StatusMessage{
				Type: serial.StatusDown, Machine: machineID,
				Message: "input not delivered: " + err.Error(), At: time.Now(),
			})
		}
	}
}

// controlStatus builds the lease status frame a client sees.
func controlStatus(machineID string, l lease.Lease, mine bool, message string) serial.StatusMessage {
	msg := serial.StatusMessage{
		Type:    serial.StatusControl,
		Machine: machineID,
		Message: message,
		Holder:  l.Holder,
		Write:   &mine,
		At:      time.Now(),
	}
	if !l.Expires.IsZero() {
		exp := l.Expires
		msg.Expires = &exp
	}
	return msg
}

func leaseMessage(ev lease.Event, mine bool) string {
	switch ev.Type {
	case lease.EventGranted:
		if mine {
			return "you have control"
		}
		return ev.Lease.Holder + " has taken control"
	case lease.EventRenewed:
		if mine {
			return "control renewed"
		}
		return ""
	case lease.EventExpiring:
		if mine {
			return "your control is about to expire"
		}
		return ""
	case lease.EventExpired:
		if mine {
			return "your control expired"
		}
		return "control is free"
	case lease.EventReleased:
		if mine {
			return "you released control"
		}
		return "control is free"
	}
	return ""
}
