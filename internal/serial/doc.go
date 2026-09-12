// Package serial implements the serial broker: one upstream connection per
// machine, fanned out to many subscribers, with scrollback and a transcript.
//
// A QEMU -chardev socket accepts a single connection, so labview holds that
// connection itself and clients subscribe to labview rather than to QEMU. See
// design section 3 for why this asymmetry exists and section 11 for the
// settled decisions about eager start and transcript rotation.
package serial

import "time"

// Defaults for the knobs described in the design. All are overridable via
// config; these are the "lab scale" values section 11 assumes.
const (
	// DefaultRingBytes is the scrollback window, per design section 3.
	DefaultRingBytes = 256 << 10

	// DefaultSubscriberQueue is how many pending chunks a subscriber may
	// fall behind by before it is dropped. Dropping and letting the client
	// reconnect into fresh scrollback is honest; silently discarding bytes
	// from the middle of a stream is not.
	DefaultSubscriberQueue = 256

	// Backoff bounds for re-establishing an upstream connection. A VM that
	// is switched off is the normal case, not an error, so the ceiling is
	// low enough that a machine being started is picked up promptly.
	DefaultBackoffMin = 250 * time.Millisecond
	DefaultBackoffMax = 5 * time.Second

	// DefaultDialTimeout bounds a single upstream dial attempt.
	DefaultDialTimeout = 5 * time.Second
)
