package serial

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestRingBelowCapacity(t *testing.T) {
	r := NewRing(16)
	r.Write([]byte("boot"))
	r.Write([]byte("ing"))
	if got := string(r.Snapshot()); got != "booting" {
		t.Fatalf("snapshot = %q, want %q", got, "booting")
	}
	if r.Len() != 7 {
		t.Fatalf("Len = %d, want 7", r.Len())
	}
}

func TestRingKeepsTailOfOversizedWrite(t *testing.T) {
	r := NewRing(4)
	r.Write([]byte("0123456789"))
	if got := string(r.Snapshot()); got != "6789" {
		t.Fatalf("snapshot = %q, want %q", got, "6789")
	}
}

func TestRingSlidesWindow(t *testing.T) {
	r := NewRing(8)
	r.Write([]byte("abcdef"))
	r.Write([]byte("ghij")) // wraps, evicts "ab"
	if got := string(r.Snapshot()); got != "cdefghij" {
		t.Fatalf("snapshot = %q, want %q", got, "cdefghij")
	}
}

func TestRingResetDropsScrollback(t *testing.T) {
	r := NewRing(8)
	r.Write([]byte("previous"))
	r.Reset()
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("snapshot after reset = %q, want empty", got)
	}
	r.Write([]byte("new"))
	if got := string(r.Snapshot()); got != "new" {
		t.Fatalf("snapshot = %q, want %q", got, "new")
	}
	// Total counts everything ever written, reset included.
	if r.Total() != 11 {
		t.Fatalf("Total = %d, want 11", r.Total())
	}
}

// TestRingMatchesNaiveModel is the one that actually earns its keep: the
// circular arithmetic is easy to get subtly wrong at the wrap boundary, so
// compare against an obviously-correct implementation over random writes.
func TestRingMatchesNaiveModel(t *testing.T) {
	const capacity = 64
	rng := rand.New(rand.NewSource(1))

	for trial := 0; trial < 200; trial++ {
		r := NewRing(capacity)
		var model []byte

		for op := 0; op < 40; op++ {
			chunk := make([]byte, rng.Intn(100))
			for i := range chunk {
				chunk[i] = byte('a' + rng.Intn(26))
			}
			r.Write(chunk)

			model = append(model, chunk...)
			if len(model) > capacity {
				model = model[len(model)-capacity:]
			}

			if got := r.Snapshot(); !bytes.Equal(got, model) {
				t.Fatalf("trial %d op %d: snapshot = %q, want %q",
					trial, op, got, model)
			}
		}
	}
}

func TestRingZeroCapacityFallsBackToDefault(t *testing.T) {
	r := NewRing(0)
	if len(r.buf) != DefaultRingBytes {
		t.Fatalf("capacity = %d, want %d", len(r.buf), DefaultRingBytes)
	}
}
