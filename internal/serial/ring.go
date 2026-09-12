package serial

// Ring is a fixed-size circular byte buffer holding the most recent output
// from a serial line.
//
// It is the scrollback: every subscriber receives a snapshot of it on attach,
// before any live bytes, which is what lets a developer who opens a machine at
// 14:02 still read the boot output from 14:01 (design section 3).
//
// Bytes, not lines. A serial stream has no framing and is not UTF-8 safe
// mid-stream, so the ring stores exactly what arrived and lets the browser
// decode incrementally.
//
// Ring is NOT safe for concurrent use, deliberately. Its owner is a Broker,
// which updates it and fans out to subscribers under a single lock -- that
// atomicity is the whole point of Attach, and a second mutex in here would
// guard the same bytes with a different lock. Reach the scrollback through
// Broker.Scrollback rather than touching a broker's ring directly.
type Ring struct {
	buf   []byte
	start int   // index of the oldest byte when full
	size  int   // bytes currently held
	total int64 // bytes ever written, for diagnostics
}

// NewRing returns a ring holding at most capacity bytes.
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultRingBytes
	}
	return &Ring{buf: make([]byte, capacity)}
}

// Write appends p, discarding the oldest bytes as needed. It never fails and
// never blocks on a subscriber, because capturing output must not depend on
// anybody watching.
func (r *Ring) Write(p []byte) (int, error) {
	r.total += int64(len(p))

	capacity := len(r.buf)
	// A write larger than the ring keeps only its tail.
	if len(p) >= capacity {
		copy(r.buf, p[len(p)-capacity:])
		r.start, r.size = 0, capacity
		return len(p), nil
	}

	end := (r.start + r.size) % capacity
	n := copy(r.buf[end:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
	}

	if r.size+len(p) <= capacity {
		r.size += len(p)
		return len(p), nil
	}
	// Overwrote the oldest bytes: the window slid forward.
	r.size = capacity
	r.start = (end + len(p)) % capacity
	return len(p), nil
}

// Snapshot returns a copy of the buffered bytes, oldest first.
func (r *Ring) Snapshot() []byte {
	out := make([]byte, r.size)
	if r.size == 0 {
		return out
	}
	capacity := len(r.buf)
	if r.start+r.size <= capacity {
		copy(out, r.buf[r.start:r.start+r.size])
		return out
	}
	n := copy(out, r.buf[r.start:])
	copy(out[n:], r.buf[:r.size-n])
	return out
}

// Len reports the number of buffered bytes.
func (r *Ring) Len() int { return r.size }

// Total reports how many bytes have ever passed through the ring.
func (r *Ring) Total() int64 { return r.total }

// Reset empties the ring.
//
// Note that a broker does *not* reset on reconnect: carrying the previous
// run's tail into the new one is what lets a developer read the panic and the
// reboot that followed it in one scrollback, which is the "why did the VM
// reboot" case. Reset is for when a machine's serial address changes in the
// inventory and the buffer therefore describes a different endpoint.
func (r *Ring) Reset() { r.start, r.size = 0, 0 }
