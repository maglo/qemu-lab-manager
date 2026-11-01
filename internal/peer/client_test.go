package peer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListMachinesRewritesIDs(t *testing.T) {
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/machines" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-Forwarded-User"); got != "alice" {
			t.Errorf("identity = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"machines": []map[string]any{
				{"id": "el9-build", "name": "el9-build", "live": true},
				{"id": "serial-only", "name": "serial-only", "host": "rack-a", "live": false},
			},
		})
	}))
	t.Cleanup(peerSrv.Close)

	client, err := New(Options{
		Peers:          []Peer{{Name: "kvm01", URL: peerSrv.URL}},
		IdentityHeader: "X-Forwarded-User",
	})
	if err != nil {
		t.Fatal(err)
	}

	machines, failed := client.ListMachines(context.Background(), "alice")
	if len(failed) != 0 {
		t.Fatalf("failed = %#v", failed)
	}
	if len(machines) != 2 {
		t.Fatalf("got %d machines", len(machines))
	}
	if machines[0]["id"] != "kvm01:el9-build" {
		t.Errorf("id = %v", machines[0]["id"])
	}
	if machines[0]["host"] != "kvm01" {
		t.Errorf("empty host was not filled: %v", machines[0]["host"])
	}
	if machines[1]["id"] != "kvm01:serial-only" {
		t.Errorf("id = %v", machines[1]["id"])
	}
	if machines[1]["host"] != "rack-a" {
		t.Errorf("existing host was overwritten: %v", machines[1]["host"])
	}
}

func TestListMachinesPeerFailure(t *testing.T) {
	client, err := New(Options{
		Peers: []Peer{{Name: "down", URL: "http://127.0.0.1:1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	machines, failed := client.ListMachines(context.Background(), "")
	if len(machines) != 0 {
		t.Fatalf("machines = %d", len(machines))
	}
	if len(failed) != 1 || failed[0].Peer != "down" {
		t.Fatalf("failed = %#v", failed)
	}
}

func TestProxyMachine(t *testing.T) {
	var sawPath, sawUser string
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawUser = r.Header.Get("X-Forwarded-User")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"el9-build","ok":true}`))
	}))
	t.Cleanup(peerSrv.Close)

	client, err := New(Options{
		Peers: []Peer{{Name: "kvm01", URL: peerSrv.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/machines/kvm01:el9-build", nil)
	req.SetPathValue("id", "kvm01:el9-build")
	rec := httptest.NewRecorder()
	if !client.ProxyMachine(rec, req, "bob") {
		t.Fatal("ProxyMachine returned false")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if sawPath != "/api/machines/el9-build" {
		t.Errorf("peer path = %q", sawPath)
	}
	if sawUser != "bob" {
		t.Errorf("identity = %q", sawUser)
	}
}

func TestProxyMachineLocalID(t *testing.T) {
	client, err := New(Options{Peers: []Peer{{Name: "kvm01", URL: "http://example"}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/machines/el9-build", nil)
	req.SetPathValue("id", "el9-build")
	rec := httptest.NewRecorder()
	if client.ProxyMachine(rec, req, "") {
		t.Fatal("a local id was proxied")
	}
}

func TestNewRejectsDuplicatePeers(t *testing.T) {
	_, err := New(Options{Peers: []Peer{
		{Name: "kvm01", URL: "http://a"},
		{Name: "kvm01", URL: "http://b"},
	}})
	if err == nil {
		t.Fatal("duplicate peer names were accepted")
	}
}
