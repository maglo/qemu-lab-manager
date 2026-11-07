package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
	"github.com/maglo/qemu-lab-manager/labview/internal/telemetry"
)

func TestTelemetryDumpAndConfig(t *testing.T) {
	h := newHarness(t)
	h.telem.NodeUp(3, map[string]any{"version": "test"})
	h.srv.activity.Record(activity.Event{
		Machine: "el9-build", User: "alice", Action: activity.ActionCreated, Detail: "ok",
	})

	rec := h.do("GET", "/api/telemetry?follow=0", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "ndjson") {
		t.Fatalf("content-type = %q", ct)
	}

	var kinds []string
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev telemetry.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		kinds = append(kinds, ev.Kind+"/"+ev.Type)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(kinds) < 2 {
		t.Fatalf("events = %v", kinds)
	}
	if kinds[0] != "node/up" {
		t.Fatalf("first = %s", kinds[0])
	}
	foundCreated := false
	for _, k := range kinds {
		if k == "machine/created" {
			foundCreated = true
		}
	}
	if !foundCreated {
		t.Fatalf("missing created: %v", kinds)
	}

	cfgRec := h.do("GET", "/api/config", "alice", nil)
	var cfg map[string]any
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["telemetry"] != true {
		t.Fatalf("config telemetry = %v", cfg["telemetry"])
	}
}

func TestTelemetryFollowSeesLiveEvent(t *testing.T) {
	h := newHarness(t)
	h.telem.NodeUp(0, nil)

	ts := httptest.NewServer(h.srv)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/telemetry", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-User", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			lines <- line
		}
		close(lines)
	}()

	sawUp := false
	deadline := time.After(2 * time.Second)
	for !sawUp {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before up")
			}
			if strings.Contains(line, `"type":"up"`) {
				sawUp = true
			}
		case <-deadline:
			t.Fatal("no up event")
		}
	}

	h.srv.activity.Record(activity.Event{
		Machine: "el9-build", User: "bob", Action: activity.ActionPower, Detail: "start",
	})

	deadline = time.After(2 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before power")
			}
			if strings.Contains(line, `"type":"power"`) {
				cancel()
				return
			}
		case <-deadline:
			t.Fatal("no power event")
		}
	}
}

func TestTelemetryPeerFailOnce(t *testing.T) {
	h := newHarness(t)
	client, err := peer.New(peer.Options{
		Peers: []peer.Peer{{Name: "kvm02", URL: "http://127.0.0.1:1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.peers = client

	for i := 0; i < 3; i++ {
		rec := h.do("GET", "/api/machines", "alice", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %d: %d", i, rec.Code)
		}
	}
	snap := h.telem.Snapshot()
	fails := 0
	for _, ev := range snap {
		if ev.Type == telemetry.TypePeerFail {
			fails++
		}
	}
	if fails != 1 {
		t.Fatalf("peer-fail count = %d, want 1; snap=%+v", fails, snap)
	}
}
