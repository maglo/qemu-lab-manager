package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
)

func TestPeerMachinesJoinTheWall(t *testing.T) {
	var proxiedPath string
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"machines": []map[string]any{
					{"id": "remote-one", "name": "remote-one", "live": true},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines/remote-one":
			proxiedPath = r.URL.Path
			_, _ = w.Write([]byte(`{"id":"remote-one"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(peerSrv.Close)

	h := newHarness(t)
	client, err := peer.New(peer.Options{
		Peers:          []peer.Peer{{Name: "kvm02", URL: peerSrv.URL}},
		IdentityHeader: h.srv.cfg.IdentityHeader,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.peers = client

	rec := h.do("GET", "/api/machines", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rec.Code, rec.Body)
	}
	var list struct {
		Machines []struct {
			ID string `json:"id"`
		} `json:"machines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range list.Machines {
		if m.ID == "kvm02:remote-one" {
			found = true
		}
	}
	if !found {
		t.Fatalf("peer machine missing: %#v", list.Machines)
	}

	rec = h.do("GET", "/api/machines/kvm02:remote-one", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", rec.Code, rec.Body)
	}
	if proxiedPath != "/api/machines/remote-one" {
		t.Errorf("proxied path = %q", proxiedPath)
	}
}

func TestPeerWebsocketRefused(t *testing.T) {
	h := newHarness(t)
	client, err := peer.New(peer.Options{
		Peers: []peer.Peer{{Name: "kvm02", URL: "http://127.0.0.1:1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.peers = client
	rec := h.do("GET", "/ws/console/kvm02:remote-one", "alice", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}
