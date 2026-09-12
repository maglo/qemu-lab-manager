package rfb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"testing"
)

// Message builders, so the tests read as protocol rather than as hex.
func version() []byte { return []byte("RFB 003.008\n") }

func securityNone() []byte { return []byte{1} }
func securityVNC() []byte  { return []byte{2} }
func vncAuthResponse() []byte {
	return bytes.Repeat([]byte{0xAB}, 16)
}
func clientInit() []byte { return []byte{1} }

func setPixelFormat() []byte {
	b := make([]byte, 20)
	b[0] = msgSetPixelFormat
	return b
}

func setEncodings(n int) []byte {
	b := make([]byte, 4+4*n)
	b[0] = msgSetEncodings
	binary.BigEndian.PutUint16(b[2:4], uint16(n))
	return b
}

func framebufferUpdateRequest() []byte {
	b := make([]byte, 10)
	b[0] = msgFramebufferUpdateRequest
	return b
}

func keyEvent(key uint32) []byte {
	b := make([]byte, 8)
	b[0] = msgKeyEvent
	b[1] = 1
	binary.BigEndian.PutUint32(b[4:8], key)
	return b
}

func pointerEvent() []byte {
	b := make([]byte, 6)
	b[0] = msgPointerEvent
	return b
}

func clientCutText(text string) []byte {
	b := make([]byte, 8+len(text))
	b[0] = msgClientCutText
	binary.BigEndian.PutUint32(b[4:8], uint32(len(text)))
	copy(b[8:], text)
	return b
}

func handshake() []byte {
	return concat(version(), securityNone(), clientInit())
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// The handshake must pass through untouched: refusing its bytes would break
// the connection rather than make it read only.
func TestHandshakePassesThroughUnchanged(t *testing.T) {
	f := NewInputFilter()
	in := handshake()
	out, err := f.Filter(in)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("handshake altered:\n got %x\nwant %x", out, in)
	}
	if f.InHandshake() {
		t.Fatal("still in handshake after ClientInit")
	}
}

func TestVNCAuthHandshakePassesThrough(t *testing.T) {
	f := NewInputFilter()
	in := concat(version(), securityVNC(), vncAuthResponse(), clientInit())
	out, err := f.Filter(in)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("authenticated handshake altered:\n got %x\nwant %x", out, in)
	}
	if f.InHandshake() {
		t.Fatal("still in handshake after ClientInit")
	}
}

// The point of the filter: a viewer keeps receiving frames but cannot type.
func TestViewingTrafficForwardedInputDropped(t *testing.T) {
	f := NewInputFilter()
	if _, err := f.Filter(handshake()); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	viewing := concat(setPixelFormat(), setEncodings(3), framebufferUpdateRequest())
	input := concat(keyEvent(0x41), pointerEvent(), clientCutText("secret"))

	out, err := f.Filter(concat(viewing, input))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if !bytes.Equal(out, viewing) {
		t.Fatalf("forwarded bytes are not exactly the viewing traffic:\n got %x\nwant %x", out, viewing)
	}
	if f.Dropped() != 3 {
		t.Fatalf("dropped = %d, want 3", f.Dropped())
	}
}

// A keystroke split across reads must not slip through in pieces.
func TestInputSplitAcrossWritesStillDropped(t *testing.T) {
	f := NewInputFilter()
	f.Filter(handshake())

	key := keyEvent(0x42)
	var out []byte
	for i := range key {
		got, err := f.Filter(key[i : i+1])
		if err != nil {
			t.Fatalf("Filter: %v", err)
		}
		out = append(out, got...)
	}
	if len(out) != 0 {
		t.Fatalf("a byte-at-a-time keystroke leaked %x", out)
	}
	if f.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", f.Dropped())
	}
}

// The same stream must filter identically however the reads happen to be
// chunked, which is the property a network gives no control over.
func TestFilteringIsIndependentOfChunking(t *testing.T) {
	stream := concat(
		handshake(),
		setPixelFormat(),
		keyEvent(0x41),
		setEncodings(2),
		pointerEvent(),
		framebufferUpdateRequest(),
		clientCutText("paste"),
		framebufferUpdateRequest(),
	)

	whole := NewInputFilter()
	want, err := whole.Filter(stream)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}

	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 200; trial++ {
		f := NewInputFilter()
		var got []byte
		for off := 0; off < len(stream); {
			n := 1 + rng.Intn(9)
			if off+n > len(stream) {
				n = len(stream) - off
			}
			chunk, err := f.Filter(stream[off : off+n])
			if err != nil {
				t.Fatalf("trial %d: %v", trial, err)
			}
			got = append(got, chunk...)
			off += n
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("trial %d: chunking changed the result\n got %x\nwant %x", trial, got, want)
		}
		if f.Dropped() != whole.Dropped() {
			t.Fatalf("trial %d: dropped = %d, want %d", trial, f.Dropped(), whole.Dropped())
		}
	}
}

// A long clipboard paste is a variable length message; its length prefix must
// actually be honoured or everything after it is misread.
func TestLongClientCutTextConsumedEntirely(t *testing.T) {
	f := NewInputFilter()
	f.Filter(handshake())

	// A paste big enough to span several reads, followed by traffic that
	// must still be recognised.
	paste := clientCutText(string(bytes.Repeat([]byte("x"), 5000)))
	after := framebufferUpdateRequest()

	out, err := f.Filter(concat(paste, after))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if !bytes.Equal(out, after) {
		t.Fatalf("framing lost after a long paste: got %x", out)
	}
}

// An unknown message type has an unknown length, so the stream can no longer
// be split. Failing closed is the only safe answer: guessing corrupts the
// session, and forwarding blindly defeats the lease.
func TestUnknownMessageFailsClosed(t *testing.T) {
	f := NewInputFilter()
	f.Filter(handshake())

	out, err := f.Filter(concat(framebufferUpdateRequest(), []byte{0xFE, 0x01, 0x02}))
	if !errors.Is(err, ErrUnframed) {
		t.Fatalf("error = %v, want ErrUnframed", err)
	}
	// Whatever was understood before the unknown type is still forwarded.
	if !bytes.Equal(out, framebufferUpdateRequest()) {
		t.Fatalf("out = %x", out)
	}
	// And it stays failed rather than resynchronising on the next read.
	if _, err := f.Filter(framebufferUpdateRequest()); !errors.Is(err, ErrUnframed) {
		t.Fatalf("filter resynchronised after losing framing: %v", err)
	}
}

func TestEmptyAndPartialReads(t *testing.T) {
	f := NewInputFilter()
	if out, err := f.Filter(nil); err != nil || len(out) != 0 {
		t.Fatalf("empty read: out=%x err=%v", out, err)
	}
	// Half a ProtocolVersion is not yet anything.
	if out, err := f.Filter(version()[:6]); err != nil || len(out) != 0 {
		t.Fatalf("partial version: out=%x err=%v", out, err)
	}
	if !f.InHandshake() {
		t.Fatal("handshake completed on half a version string")
	}
}

// A SetEncodings with no encodings is legal and has no payload.
func TestZeroLengthSetEncodings(t *testing.T) {
	f := NewInputFilter()
	f.Filter(handshake())
	out, err := f.Filter(concat(setEncodings(0), keyEvent(1)))
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if !bytes.Equal(out, setEncodings(0)) {
		t.Fatalf("out = %x", out)
	}
	if f.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", f.Dropped())
	}
}

// Split must report every message, input included, so a caller holding the
// lease can forward the lot while the parser keeps its place. Losing the
// lease mid-session then just changes what the caller does with the same
// messages.
func TestSplitReportsInputAndViewingInOrder(t *testing.T) {
	f := NewInputFilter()
	f.Filter(handshake())

	stream := concat(framebufferUpdateRequest(), keyEvent(0x41), setEncodings(1), pointerEvent())
	msgs, err := f.Split(stream)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4", len(msgs))
	}
	wantInput := []bool{false, true, false, true}
	var rebuilt []byte
	for i, m := range msgs {
		if m.IsInput != wantInput[i] {
			t.Errorf("message %d IsInput = %v, want %v", i, m.IsInput, wantInput[i])
		}
		rebuilt = append(rebuilt, m.Bytes...)
	}
	// Nothing added, nothing lost: forwarding every message reproduces the
	// client's stream byte for byte, which is what a lease holder needs.
	if !bytes.Equal(rebuilt, stream) {
		t.Fatalf("reassembled stream differs:\n got %x\nwant %x", rebuilt, stream)
	}
}

// The framing state must survive a stretch of messages that were forwarded
// wholesale, which is what happens while the lease is held.
func TestSplitKeepsFramingAcrossLeaseChange(t *testing.T) {
	f := NewInputFilter()
	f.Filter(handshake())

	// Pretend the lease is held: everything is forwarded, but still parsed.
	if _, err := f.Split(concat(keyEvent(1), clientCutText("hello"), pointerEvent())); err != nil {
		t.Fatalf("Split while holding lease: %v", err)
	}

	// Lease lost. The next input must still be recognised and dropped.
	out, err := f.Filter(concat(keyEvent(2), framebufferUpdateRequest()))
	if err != nil {
		t.Fatalf("Filter after losing lease: %v", err)
	}
	if !bytes.Equal(out, framebufferUpdateRequest()) {
		t.Fatalf("framing lost across the lease change: got %x", out)
	}
}
