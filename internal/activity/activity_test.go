package activity

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRecordAndReadNewestFirst(t *testing.T) {
	l := New(10, discard())
	base := time.Now()
	l.Record(Event{Machine: "vm1", User: "alice", Action: ActionAttach, At: base})
	l.Record(Event{Machine: "vm1", User: "alice", Action: ActionGranted, At: base.Add(time.Second)})

	got := l.Machine("vm1")
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	// Newest first, so the tab reads as a history.
	if got[0].Action != ActionGranted {
		t.Errorf("first event = %q, want the newest", got[0].Action)
	}
}

func TestMachineHistoriesAreSeparate(t *testing.T) {
	l := New(10, discard())
	l.Record(Event{Machine: "vm1", User: "alice", Action: ActionAttach})
	l.Record(Event{Machine: "vm2", User: "bob", Action: ActionAttach})

	if got := l.Machine("vm1"); len(got) != 1 || got[0].User != "alice" {
		t.Errorf("vm1 history = %+v", got)
	}
	if got := l.Machine("vm2"); len(got) != 1 || got[0].User != "bob" {
		t.Errorf("vm2 history = %+v", got)
	}
	if got := l.Machine("absent"); len(got) != 0 {
		t.Errorf("unknown machine has history: %+v", got)
	}
	// The lab-wide view has both.
	if got := l.Recent(); len(got) != 2 {
		t.Errorf("recent = %+v", got)
	}
}

// The log is bounded: a busy machine must not grow it without limit.
func TestRecordIsBounded(t *testing.T) {
	l := New(5, discard())
	for i := 0; i < 100; i++ {
		l.Record(Event{Machine: "vm1", User: "alice", Action: ActionAttach, Detail: string(rune('a' + i%26))})
	}
	if got := l.Machine("vm1"); len(got) != 5 {
		t.Fatalf("kept %d events, want 5", len(got))
	}
	// And it kept the newest, not the oldest.
	want := string(rune('a' + 99%26))
	if got := l.Machine("vm1")[0].Detail; got != want {
		t.Errorf("newest detail = %q, want %q", got, want)
	}
}

func TestRecordStampsTime(t *testing.T) {
	l := New(5, discard())
	l.Record(Event{Machine: "vm1", User: "alice", Action: ActionAttach})
	if l.Machine("vm1")[0].At.IsZero() {
		t.Fatal("event has no timestamp")
	}
}

func TestZeroCapacityFallsBackToDefault(t *testing.T) {
	l := New(0, discard())
	if l.capacity != DefaultCapacity {
		t.Fatalf("capacity = %d, want %d", l.capacity, DefaultCapacity)
	}
}
