package peer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseFlag(t *testing.T) {
	p, err := ParseFlag("kvm01=https://lab.example/labview/")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "kvm01" || p.URL != "https://lab.example/labview" {
		t.Fatalf("%+v", p)
	}
}

func TestParseFlagRejects(t *testing.T) {
	for _, v := range []string{"", "noequals", "=http://x", "kvm01=", "kvm01=ftp://x", "bad name=http://x"} {
		if _, err := ParseFlag(v); err == nil {
			t.Errorf("%q was accepted", v)
		}
	}
}

func TestJoinSplitID(t *testing.T) {
	id := JoinID("kvm01", "el9-build")
	peer, local, ok := SplitID(id)
	if !ok || peer != "kvm01" || local != "el9-build" {
		t.Fatalf("%q %q %v", peer, local, ok)
	}
	if _, _, ok := SplitID("el9-build"); ok {
		t.Fatal("local id treated as composite")
	}
}

func TestListAndProxy(t *testing.T) {
	var sawPath, sawUser, sawToken string
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/machines":
			if got := r.Header.Get("X-Forwarded-User"); got != "alice" {
				t.Errorf("identity = %q", got)
			}
			if got := r.Header.Get(ClusterTokenHeader); got != "secret" {
				t.Errorf("token = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"machines": []map[string]any{
					{"id": "el9", "name": "el9", "live": true},
				},
			})
		case "/api/machines/el9":
			sawPath = r.URL.Path
			sawUser = r.Header.Get("X-Forwarded-User")
			sawToken = r.Header.Get(ClusterTokenHeader)
			_, _ = w.Write([]byte(`{"id":"el9"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(peerSrv.Close)

	client, err := New(Options{
		Peers:          []Peer{{Name: "kvm01", URL: peerSrv.URL}},
		IdentityHeader: "X-Forwarded-User",
		ClusterToken:   "secret",
	})
	if err != nil {
		t.Fatal(err)
	}

	machines, failed := client.ListMachines(context.Background(), "alice")
	if len(failed) != 0 || len(machines) != 1 || machines[0]["id"] != "kvm01:el9" {
		t.Fatalf("machines=%v failed=%v", machines, failed)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/machines/kvm01:el9", nil)
	req.SetPathValue("id", "kvm01:el9")
	rec := httptest.NewRecorder()
	if !client.ProxyMachine(rec, req, "bob") {
		t.Fatal("not proxied")
	}
	if sawPath != "/api/machines/el9" || sawUser != "bob" || sawToken != "secret" {
		t.Fatalf("path=%q user=%q token=%q", sawPath, sawUser, sawToken)
	}
}

func TestNewRejectsDuplicates(t *testing.T) {
	_, err := New(Options{Peers: []Peer{
		{Name: "a", URL: "http://a"},
		{Name: "a", URL: "http://b"},
	}})
	if err == nil {
		t.Fatal("duplicate accepted")
	}
}
