package telemetry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEmitRingAndStdout(t *testing.T) {
	var out bytes.Buffer
	b := New(Options{Capacity: 3, Stdout: &out})
	for i := 0; i < 5; i++ {
		b.Emit(Event{Kind: KindMachine, Type: TypePower, Machine: "m", Detail: string(rune('a' + i))})
	}
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("ring len = %d, want 3", len(snap))
	}
	if snap[0].Detail != "c" || snap[2].Detail != "e" {
		t.Fatalf("ring = %+v", snap)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("stdout lines = %d, want 5", len(lines))
	}
	var last Event
	if err := json.Unmarshal([]byte(lines[4]), &last); err != nil {
		t.Fatal(err)
	}
	if last.Detail != "e" || last.At.IsZero() {
		t.Fatalf("last = %+v", last)
	}
}

func TestSubscribe(t *testing.T) {
	b := New(Options{Capacity: 10})
	ch := b.Subscribe(8)
	b.Emit(Event{Kind: KindNode, Type: TypeUp, Fields: map[string]any{"machines": 2}})
	select {
	case ev := <-ch:
		if ev.Type != TypeUp || ev.Fields["machines"] != 2 {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	b.Unsubscribe(ch)
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel still open")
		}
	default:
		t.Fatal("channel not closed")
	}
}

func TestObserveActivity(t *testing.T) {
	b := New(Options{Capacity: 10})
	b.ObserveActivity("el9", "alice", "control-granted", "")
	b.ObserveActivity("el9", "alice", "created", "disk")
	snap := b.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len = %d", len(snap))
	}
	if snap[0].Type != TypeLease || snap[0].Kind != KindMachine {
		t.Fatalf("lease = %+v", snap[0])
	}
	if snap[1].Type != TypeCreated || snap[1].Detail != "disk" {
		t.Fatalf("created = %+v", snap[1])
	}
}

func TestNodeUpAndPeerFail(t *testing.T) {
	b := New(Options{Capacity: 10})
	b.NodeUp(3, map[string]any{"diskFreeBytes": int64(100)})
	b.PeerFail("kvm02", "timeout")
	snap := b.Snapshot()
	if snap[0].Kind != KindNode || snap[0].Type != TypeUp {
		t.Fatalf("up = %+v", snap[0])
	}
	if snap[0].Fields["machines"] != 3 {
		t.Fatalf("machines field = %v", snap[0].Fields["machines"])
	}
	if snap[1].Type != TypePeerFail || snap[1].Fields["peer"] != "kvm02" {
		t.Fatalf("peer = %+v", snap[1])
	}
}

// A subscriber that leaves while another goroutine emits is a client that
// closes the telemetry stream on a busy node.
func TestUnsubscribeDuringEmit(t *testing.T) {
	b := New(Options{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			b.Emit(Event{Kind: "test"})
		}
	}()

	for {
		select {
		case <-done:
			return
		default:
		}
		b.Unsubscribe(b.Subscribe(1))
	}
}
