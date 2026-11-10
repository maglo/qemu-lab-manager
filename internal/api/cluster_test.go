package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
)

// clusterHarness is an API node with a cluster token and no flag peer.
func clusterHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.ClusterToken = "secret"
	client, err := peer.New(peer.Options{
		IdentityHeader: h.srv.cfg.IdentityHeader,
		ClusterToken:   "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.peers = client
	return h
}

func (h *harness) withToken(method, path, token string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if token != "" {
		req.Header.Set(peer.ClusterTokenHeader, token)
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec
}

// A node that registers through the real joiner puts its machines on the
// wall of the API node, with no restart and no -peer flag.
func TestNodeJoinsAtRunTime(t *testing.T) {
	var sawToken string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/machines" {
			http.NotFound(w, r)
			return
		}
		sawToken = r.Header.Get(peer.ClusterTokenHeader)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"machines": []map[string]any{{"id": "guest", "name": "guest"}},
		})
	}))
	t.Cleanup(node.Close)

	h := clusterHarness(t)
	apiNode := httptest.NewServer(h.srv)
	t.Cleanup(apiNode.Close)

	j := &peer.Joiner{
		APIURL: apiNode.URL,
		Token:  "secret",
		Self:   peer.Registration{Name: "node2", URL: node.URL, KVM: true},
	}
	if err := j.Once(context.Background()); err != nil {
		t.Fatalf("join: %v", err)
	}

	list := decode[struct {
		Machines []struct {
			ID string `json:"id"`
		} `json:"machines"`
	}](t, h.do("GET", "/api/machines", "alice", nil))
	found := false
	for _, m := range list.Machines {
		found = found || m.ID == "node2:guest"
	}
	if !found {
		t.Fatalf("the machine of the node is not on the wall: %+v", list.Machines)
	}
	if sawToken != "secret" {
		t.Errorf("the API node called the node with token %q", sawToken)
	}

	nodes := decode[struct {
		Nodes []peer.Node `json:"nodes"`
	}](t, h.do("GET", "/api/cluster/nodes", "alice", nil))
	if len(nodes.Nodes) != 1 || nodes.Nodes[0].Name != "node2" || !nodes.Nodes[0].Ready {
		t.Fatalf("nodes = %+v", nodes.Nodes)
	}
}

func TestRegistrationNeedsTheToken(t *testing.T) {
	h := clusterHarness(t)
	reg := peer.Registration{Name: "node2", URL: "http://10.0.0.2:8080", KVM: true}

	for _, token := range []string{"", "wrong"} {
		rec := h.withToken("POST", "/api/cluster/nodes", token, reg)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401: %s", token, rec.Code, rec.Body)
		}
	}
	if h.srv.peers.Len() != 0 {
		t.Fatal("a node joined with no valid token")
	}

	if rec := h.withToken("POST", "/api/cluster/nodes", "secret", reg); rec.Code != http.StatusOK {
		t.Fatalf("status = %d with the token: %s", rec.Code, rec.Body)
	}
	if h.srv.peers.Len() != 1 {
		t.Fatal("a node with the token did not join")
	}
}

func TestRegistrationIsOffWithNoToken(t *testing.T) {
	h := newHarness(t)
	client, err := peer.New(peer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.peers = client

	rec := h.withToken("POST", "/api/cluster/nodes", "", peer.Registration{Name: "n", URL: "http://n:1"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
	}
	if h.srv.peers.Len() != 0 {
		t.Fatal("a node joined a lab that has no token")
	}
}

func TestRegistrationRejectsBadBodies(t *testing.T) {
	h := clusterHarness(t)
	for name, body := range map[string]any{
		"no url":     peer.Registration{Name: "n"},
		"bad name":   peer.Registration{Name: "a:b", URL: "http://n:1"},
		"bad scheme": peer.Registration{Name: "n", URL: "file:///etc"},
	} {
		if rec := h.withToken("POST", "/api/cluster/nodes", "secret", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

// A call that carries a wrong token comes from a node of another cluster.
// A call with no token is a browser behind the proxy.
func TestWrongClusterTokenIsRefusedOnEveryPath(t *testing.T) {
	h := clusterHarness(t)

	for _, path := range []string{"/api/machines", "/api/config", "/healthz", "/"} {
		if rec := h.withToken("GET", path, "wrong", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token: status = %d, want 401", path, rec.Code)
		}
		if rec := h.withToken("GET", path, "secret", nil); rec.Code != http.StatusOK {
			t.Errorf("%s with the token: status = %d, want 200", path, rec.Code)
		}
		if rec := h.withToken("GET", path, "", nil); rec.Code != http.StatusOK {
			t.Errorf("%s with no token: status = %d, want 200", path, rec.Code)
		}
	}
}

func TestTokenIsRefusedByANodeWithNoToken(t *testing.T) {
	h := newHarness(t)
	if rec := h.withToken("GET", "/api/machines", "anything", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
