// Package rfb understands just enough of the RFB protocol to tell a client's
// input apart from its viewing traffic.
//
// The design gates the keyboard on the write lease, and flips noVNC's viewOnly
// off once the lease is held (design sections 4 and 7). That is the right UI,
// but it is the browser policing itself: two tabs, a stale page or a scripted
// client would still be able to type. This filter makes the same rule true on
// the server.
//
// It is deliberately not a proxy or a decoder. Server to client bytes are
// never touched -- the framebuffer stream stays pure binary with its own
// handshake, and nothing may be injected into it (design section 11). Only the
// client to server direction is parsed, and only far enough to find message
// boundaries.
package rfb

import (
	"encoding/binary"
	"errors"
)

// Client to server message types, RFB 3.8 section 7.5.
const (
	msgSetPixelFormat           = 0
	msgSetEncodings             = 2
	msgFramebufferUpdateRequest = 3
	msgKeyEvent                 = 4
	msgPointerEvent             = 5
	msgClientCutText            = 6
)

// phase tracks where in the protocol the client stream is. The handshake has
// its own framing, and it must pass through untouched.
type phase int

const (
	phaseVersion    phase = iota // 12 byte ProtocolVersion
	phaseSecurity                // 1 byte chosen security type
	phaseVNCAuth                 // 16 byte challenge response, VNC auth only
	phaseClientInit              // 1 byte shared flag
	phaseMessages                // normal message stream
)

// ErrUnframed means the filter lost track of message boundaries, which
// happens when a client uses an extension the filter does not know. The
// caller must stop forwarding client bytes: guessing would corrupt the
// session, and letting them through unfiltered would defeat the lease.
var ErrUnframed = errors.New("rfb: unrecognised client message, framing lost")

// Message is one complete client to server message.
type Message struct {
	// Bytes is the message exactly as the client sent it.
	Bytes []byte
	// IsInput marks a message that drives the machine -- a key event, a
	// pointer event or a clipboard write -- as opposed to viewing traffic
	// the machine is unaffected by.
	IsInput bool
}

// InputFilter splits a client's byte stream into what is safe to forward when
// the client does not hold the write lease, and what is input.
//
// It is stateful and must be fed every client byte in order. It is not safe
// for concurrent use; one filter belongs to one connection.
type InputFilter struct {
	phase phase
	buf   []byte

	// dropped counts suppressed input messages, for logging and for telling
	// a client why nothing is happening.
	dropped int
}

// NewInputFilter returns a filter positioned at the start of a session.
func NewInputFilter() *InputFilter { return &InputFilter{phase: phaseVersion} }

// Dropped reports how many input messages have been suppressed.
func (f *InputFilter) Dropped() int { return f.dropped }

// InHandshake reports whether the handshake is still in progress. While it
// is, everything is forwarded: refusing handshake bytes would break the
// connection rather than make it read only.
func (f *InputFilter) InHandshake() bool { return f.phase != phaseMessages }

// Split consumes client bytes and returns the complete messages they contain,
// in order, each marked as input or as viewing traffic. Bytes forming only
// part of a message are held until the rest arrives.
//
// Every client byte must go through Split even while the client does hold the
// lease. The parser is stateful, and a message forwarded without being parsed
// would leave it unable to find the next boundary -- so a developer who lost
// their lease mid-session would take the filter's framing down with it.
// Handshake bytes come back as viewing traffic, because refusing them would
// break the connection rather than make it read only.
func (f *InputFilter) Split(p []byte) ([]Message, error) {
	f.buf = append(f.buf, p...)
	var out []Message

	for {
		n, forward, err := f.next()
		if err != nil {
			return out, err
		}
		if n == 0 {
			return out, nil // need more bytes
		}
		msg := Message{Bytes: append([]byte(nil), f.buf[:n]...), IsInput: !forward}
		if msg.IsInput {
			f.dropped++
		}
		out = append(out, msg)
		f.buf = f.buf[n:]
	}
}

// Filter is Split reduced to the bytes a client with no write lease may send
// upstream: viewing traffic through, input dropped.
func (f *InputFilter) Filter(p []byte) ([]byte, error) {
	msgs, err := f.Split(p)
	var out []byte
	for _, m := range msgs {
		if !m.IsInput {
			out = append(out, m.Bytes...)
		}
	}
	return out, err
}

// next examines the head of the buffer and reports the length of the message
// there, whether to forward it, and whether framing was lost. A length of
// zero means the message is incomplete.
func (f *InputFilter) next() (int, bool, error) {
	switch f.phase {
	case phaseVersion:
		if len(f.buf) < 12 {
			return 0, false, nil
		}
		f.phase = phaseSecurity
		return 12, true, nil

	case phaseSecurity:
		if len(f.buf) < 1 {
			return 0, false, nil
		}
		// Security type 2 is VNC authentication, which is the only common
		// type that adds a client side message. Everything else moves
		// straight on to ClientInit.
		if f.buf[0] == 2 {
			f.phase = phaseVNCAuth
		} else {
			f.phase = phaseClientInit
		}
		return 1, true, nil

	case phaseVNCAuth:
		if len(f.buf) < 16 {
			return 0, false, nil
		}
		f.phase = phaseClientInit
		return 16, true, nil

	case phaseClientInit:
		if len(f.buf) < 1 {
			return 0, false, nil
		}
		f.phase = phaseMessages
		return 1, true, nil
	}

	// Message phase.
	if len(f.buf) < 1 {
		return 0, false, nil
	}
	switch f.buf[0] {
	case msgSetPixelFormat:
		return needed(f.buf, 20, true)
	case msgSetEncodings:
		if len(f.buf) < 4 {
			return 0, false, nil
		}
		count := int(binary.BigEndian.Uint16(f.buf[2:4]))
		return needed(f.buf, 4+4*count, true)
	case msgFramebufferUpdateRequest:
		return needed(f.buf, 10, true)
	case msgKeyEvent:
		return needed(f.buf, 8, false)
	case msgPointerEvent:
		return needed(f.buf, 6, false)
	case msgClientCutText:
		if len(f.buf) < 8 {
			return 0, false, nil
		}
		length := int(binary.BigEndian.Uint32(f.buf[4:8]))
		if length < 0 {
			return 0, false, ErrUnframed
		}
		return needed(f.buf, 8+length, false)
	default:
		// An extension the filter does not know. Its length is unknown, so
		// the stream can no longer be split reliably.
		return 0, false, ErrUnframed
	}
}

func needed(buf []byte, size int, forward bool) (int, bool, error) {
	if size < 0 {
		return 0, false, ErrUnframed
	}
	if len(buf) < size {
		return 0, false, nil
	}
	return size, forward, nil
}
