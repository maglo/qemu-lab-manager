package peer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegisterAddsRenewsAndExpires(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	c, err := New(Options{
		Peers:   []Peer{{Name: "fixed", URL: "http://fixed:8080"}},
		NodeTTL: time.Minute,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	isNew, err := c.Register(Registration{Name: "node2", URL: "http://10.0.0.2:8080/", KVM: true})
	if err != nil || !isNew {
		t.Fatalf("first registration: isNew=%v err=%v", isNew, err)
	}
	if p, ok := c.Lookup("node2"); !ok || p.URL != "http://10.0.0.2:8080" {
		t.Fatalf("lookup = %+v, %v", p, ok)
	}
	if got := c.Names(); len(got) != 2 || got[0] != "fixed" || got[1] != "node2" {
		t.Fatalf("names = %v", got)
	}

	now = now.Add(45 * time.Second)
	if isNew, _ := c.Register(Registration{Name: "node2", URL: "http://10.0.0.2:8080", KVM: true}); isNew {
		t.Fatal("a renewal counted as a new node")
	}
	now = now.Add(45 * time.Second)
	if c.Len() != 2 {
		t.Fatal("a renewed node expired at the first TTL")
	}

	now = now.Add(61 * time.Second)
	if _, ok := c.Lookup("node2"); ok {
		t.Fatal("a node that stopped its registration stayed")
	}
	if c.Len() != 1 {
		t.Fatalf("len = %d, want the flag peer only", c.Len())
	}
	if isNew, _ := c.Register(Registration{Name: "node2", URL: "http://10.0.0.2:8080"}); !isNew {
		t.Fatal("a node that comes back did not count as new")
	}
}

func TestRegisterRejectsBadRegistrations(t *testing.T) {
	c, err := New(Options{Peers: []Peer{{Name: "fixed", URL: "http://fixed:8080"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Register(Registration{Name: "fixed", URL: "http://evil:1"}); !errors.Is(err, ErrFlagPeer) {
		t.Fatalf("a registration took the name of a flag peer: %v", err)
	}
	if p, _ := c.Lookup("fixed"); p.URL != "http://fixed:8080" {
		t.Fatalf("the flag peer changed: %+v", p)
	}
	for _, r := range []Registration{
		{Name: "", URL: "http://a:1"},
		{Name: "a:b", URL: "http://a:1"},
		{Name: "a", URL: "ftp://a"},
		{Name: "a", URL: ""},
	} {
		if _, err := c.Register(r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestNodesReportsKVMAndReady(t *testing.T) {
	c, err := New(Options{Peers: []Peer{{Name: "fixed", URL: "http://fixed:8080"}}})
	if err != nil {
		t.Fatal(err)
	}
	c.Register(Registration{Name: "slow", URL: "http://slow:8080", KVM: false})
	c.Register(Registration{Name: "fast", URL: "http://fast:8080", KVM: true})

	nodes := c.Nodes()
	if len(nodes) != 3 {
		t.Fatalf("nodes = %+v", nodes)
	}
	if n := nodes[0]; n.Name != "fixed" || n.Source != "flag" || n.KVM != nil || !n.Ready {
		t.Errorf("flag peer = %+v", n)
	}
	if n := nodes[1]; n.Name != "fast" || n.Source != "join" || n.KVM == nil || !*n.KVM || !n.Ready {
		t.Errorf("node with KVM = %+v", n)
	}
	if n := nodes[2]; n.Name != "slow" || n.KVM == nil || *n.KVM || n.Ready {
		t.Errorf("node without KVM is ready: %+v", n)
	}
	raw, _ := json.Marshal(nodes)
	if strings.Contains(string(raw), "8080") {
		t.Errorf("the node list shows an address: %s", raw)
	}
}

func TestJoinerSendsRegistrationWithToken(t *testing.T) {
	var got Registration
	var token string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != NodesPath {
			http.NotFound(w, r)
			return
		}
		token = r.Header.Get(ClusterTokenHeader)
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(api.Close)

	j := &Joiner{
		APIURL: api.URL,
		Token:  "secret",
		Self:   Registration{Name: "node2", URL: "http://10.0.0.2:8080", KVM: true},
	}
	if err := j.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if token != "secret" || got != j.Self {
		t.Fatalf("token=%q registration=%+v", token, got)
	}
}

func TestJoinerReportsRefusal(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"a node needs the cluster token to join"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(api.Close)

	j := &Joiner{APIURL: api.URL, Token: "wrong", Self: Registration{Name: "n", URL: "http://n:1"}}
	if err := j.Once(context.Background()); err == nil {
		t.Fatal("a refused registration reported no error")
	}
}

func TestJoinerRunRenewsUntilCancelled(t *testing.T) {
	calls := make(chan struct{}, 16)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls <- struct{}{}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(api.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	j := &Joiner{APIURL: api.URL, Token: "t", Self: Registration{Name: "n", URL: "http://n:1"}, Interval: 10 * time.Millisecond}
	go func() { j.Run(ctx); close(done) }()

	for i := 0; i < 3; i++ {
		select {
		case <-calls:
		case <-time.After(3 * time.Second):
			t.Fatalf("registration %d did not arrive", i+1)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestKVMPresentReadsThePaths(t *testing.T) {
	old := kvmPaths
	t.Cleanup(func() { kvmPaths = old })

	dir := t.TempDir()
	kvmPaths = []string{filepath.Join(dir, "kvm"), filepath.Join(dir, "kvm_intel")}
	if KVMPresent() {
		t.Fatal("KVM reported on a host with no path")
	}
	if err := os.Mkdir(filepath.Join(dir, "kvm_intel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !KVMPresent() {
		t.Fatal("KVM not reported with the module directory present")
	}
}
