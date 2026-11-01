package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/config"
	"github.com/maglo/qemu-lab-manager/labview/internal/host"
	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
	"github.com/maglo/qemu-lab-manager/labview/internal/serial"
)

// TestPeerMachinesJoinTheWall checks the first aggregation slice: peer
// machines appear on GET /api/machines, and a machine record proxies.
func TestPeerMachinesJoinTheWall(t *testing.T) {
	var proxiedPath, proxiedUser string
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"machines": []map[string]any{
					{
						"id": "remote-one", "name": "remote-one",
						"hasConsole": true, "live": true,
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines/remote-one":
			proxiedPath = r.URL.Path
			proxiedUser = r.Header.Get("X-Forwarded-User")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "remote-one", "name": "remote-one", "live": true,
				"details": map[string]any{"warnings": []string{"from peer"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(peerSrv.Close)

	h := newPeerHarness(t, peer.Peer{Name: "kvm02", URL: peerSrv.URL})

	rec := h.do("GET", "/api/machines", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rec.Code, rec.Body)
	}
	var list struct {
		Machines []struct {
			ID   string `json:"id"`
			Live bool   `json:"live"`
		} `json:"machines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range list.Machines {
		if m.ID == "kvm02:remote-one" {
			found = true
			if !m.Live {
				t.Error("remote machine should be live")
			}
		}
	}
	if !found {
		t.Fatalf("peer machine missing from %#v", list.Machines)
	}

	rec = h.do("GET", "/api/machines/kvm02:remote-one", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", rec.Code, rec.Body)
	}
	if proxiedPath != "/api/machines/remote-one" {
		t.Errorf("proxied path = %q", proxiedPath)
	}
	if proxiedUser != "alice" {
		t.Errorf("proxied identity = %q", proxiedUser)
	}
}

func TestPeerWebsocketRefused(t *testing.T) {
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(peerSrv.Close)

	h := newPeerHarness(t, peer.Peer{Name: "kvm02", URL: peerSrv.URL})
	rec := h.do("GET", "/ws/console/kvm02:remote-one", "alice", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

func newPeerHarness(t *testing.T, peers ...peer.Peer) *harness {
	t.Helper()

	dir := t.TempDir()
	invDir := filepath.Join(dir, "inventory.d")
	if err := os.MkdirAll(invDir, 0o750); err != nil {
		t.Fatal(err)
	}
	doc := "name: local-one\nhost: here\nvnc: 127.0.0.1:5901\n"
	if err := os.WriteFile(filepath.Join(invDir, "local-one.yaml"), []byte(doc), 0o640); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	w, err := inventory.NewWatcher(invDir, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })

	cfg := config.Default()
	cfg.InventoryDir = invDir
	cfg.TranscriptDir = ""
	cfg.TranscriptMaxFiles = 0
	cfg.HostAccess = config.HostFake
	cfg.Peers = peers

	ctx, cancel := context.WithCancel(context.Background())
	brokers := serial.NewManager(ctx, serial.ManagerConfig{
		BackoffMin: time.Hour,
		BackoffMax: time.Hour,
		Log:        log,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("refused")
		},
	})
	brokers.Reconcile(w.Current())

	client, err := peer.New(peer.Options{
		Peers:          peers,
		IdentityHeader: cfg.IdentityHeader,
	})
	if err != nil {
		t.Fatal(err)
	}

	leases := lease.NewManager(lease.Options{IdleTimeout: cfg.LeaseIdle, WarnBefore: cfg.LeaseWarn})
	srv := New(Deps{
		Config:    cfg,
		Inventory: w,
		Brokers:   brokers,
		Leases:    leases,
		Hosts:     host.NewFake(),
		Peers:     client,
		Activity:  activity.New(20, log),
		Log:       log,
	})

	h := &harness{srv: srv, leases: leases, brokers: brokers, cancel: cancel}
	t.Cleanup(func() { cancel(); brokers.Close() })
	return h
}
