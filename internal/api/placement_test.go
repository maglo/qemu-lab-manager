package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
)

// fakeNode is a peer that reports a capacity and records each create.
type fakeNode struct {
	*httptest.Server
	mu      sync.Mutex
	free    peer.Capacity
	creates []map[string]any
	users   []string
	tokens  []string
}

func newFakeNode(t *testing.T, free peer.Capacity) *fakeNode {
	t.Helper()
	n := &fakeNode{free: free}
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == peer.CapacityPath:
			_ = json.NewEncoder(w).Encode(n.free)
		case r.Method == http.MethodPost && r.URL.Path == "/api/machines":
			var doc map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &doc)
			n.creates = append(n.creates, doc)
			n.users = append(n.users, r.Header.Get("X-Forwarded-User"))
			n.tokens = append(n.tokens, r.Header.Get(peer.ClusterTokenHeader))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": doc["id"], "name": doc["id"]})
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines":
			_, _ = w.Write([]byte(`{"machines":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(n.Close)
	return n
}

func (n *fakeNode) created() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.creates)
}

var roomy = peer.Capacity{Lifecycle: true, KVM: true, CPUs: 8, MemoryFreeMB: 16000, DiskFreeGB: 200}

// placementHarness is an API node with create on, a set capacity, and
// the given nodes registered.
func placementHarness(t *testing.T, local peer.Capacity, nodes map[string]*fakeNode) *harness {
	t.Helper()
	h := clusterHarness(t)
	enableLifecycle(t, h)
	h.srv.capacity = func() peer.Capacity { return local }
	for name, n := range nodes {
		if _, err := h.srv.peers.Register(peer.Registration{Name: name, URL: n.URL, KVM: true}); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func machineDoc(id string) map[string]any {
	return map[string]any{"id": id, "memoryMb": 2048, "vcpus": 2, "diskGb": 20}
}

func localHas(h *harness, id string) bool {
	_, ok := h.srv.inventory.Current().Get(id)
	if ok {
		return true
	}
	// The watcher reads the directory again on its own time.
	h.srv.inventory.Rescan()
	_, ok = h.srv.inventory.Current().Get(id)
	return ok
}

func TestCreateGoesToTheNodeWithTheMostFreeMemory(t *testing.T) {
	small := roomy
	small.MemoryFreeMB = 4000
	big := newFakeNode(t, roomy)
	h := placementHarness(t, small, map[string]*fakeNode{"node2": big})

	rec := h.do("POST", "/api/machines", "alice", machineDoc("vm1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if big.created() != 1 {
		t.Fatal("the node with more free memory did not get the machine")
	}
	got := decode[struct {
		ID   string `json:"id"`
		Host string `json:"host"`
	}](t, rec)
	if got.ID != "node2:vm1" || got.Host != "node2" {
		t.Errorf("answer = %+v, want the composite id", got)
	}
	if big.users[0] != "alice" || big.tokens[0] != "secret" {
		t.Errorf("forward carried user %q and token %q", big.users[0], big.tokens[0])
	}
	if _, has := big.creates[0]["node"]; has {
		t.Error("the forwarded document carries a node field")
	}
}

func TestCreateStaysLocalWhenThisNodeHasTheMostFreeMemory(t *testing.T) {
	small := roomy
	small.MemoryFreeMB = 4000
	other := newFakeNode(t, small)
	h := placementHarness(t, roomy, map[string]*fakeNode{"node2": other})

	rec := h.do("POST", "/api/machines", "alice", machineDoc("vm1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if other.created() != 0 {
		t.Fatal("the machine went to the node with less free memory")
	}
	if got := decode[struct{ ID string }](t, rec); got.ID != "vm1" {
		t.Errorf("id = %q, want the local id", got.ID)
	}
}

func TestCreateSkipsNodesThatDoNotFit(t *testing.T) {
	for name, tight := range map[string]peer.Capacity{
		"memory":    {Lifecycle: true, KVM: true, CPUs: 8, MemoryFreeMB: 1000, DiskFreeGB: 200},
		"cpu":       {Lifecycle: true, KVM: true, CPUs: 1, MemoryFreeMB: 64000, DiskFreeGB: 200},
		"disk":      {Lifecycle: true, KVM: true, CPUs: 8, MemoryFreeMB: 64000, DiskFreeGB: 5},
		"no kvm":    {Lifecycle: true, KVM: false, CPUs: 8, MemoryFreeMB: 64000, DiskFreeGB: 200},
		"no create": {Lifecycle: false, KVM: true, CPUs: 8, MemoryFreeMB: 64000, DiskFreeGB: 200},
	} {
		t.Run(name, func(t *testing.T) {
			fits := newFakeNode(t, roomy)
			h := placementHarness(t, tight, map[string]*fakeNode{"node2": fits})
			rec := h.do("POST", "/api/machines", "alice", machineDoc("vm1"))
			if rec.Code != http.StatusCreated || fits.created() != 1 {
				t.Fatalf("status = %d, creates on the node that fits = %d: %s",
					rec.Code, fits.created(), rec.Body)
			}
		})
	}
}

func TestCreateFailsAndNamesTheShortResource(t *testing.T) {
	tightLocal := roomy
	tightLocal.MemoryFreeMB = 512
	tightDisk := roomy
	tightDisk.DiskFreeGB = 3
	node := newFakeNode(t, tightDisk)
	h := placementHarness(t, tightLocal, map[string]*fakeNode{"node2": node})
	// A registered node with no KVM is not asked at all.
	if _, err := h.srv.peers.Register(peer.Registration{Name: "node3", URL: "http://127.0.0.1:1", KVM: false}); err != nil {
		t.Fatal(err)
	}

	rec := h.do("POST", "/api/machines", "alice", machineDoc("vm1"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	msg := decode[errorBody](t, rec).Error
	for _, want := range []string{
		"local: memory: 512 MB free, the machine needs 2048 MB",
		"node2: disk: 3 GB free, the machine needs 20 GB",
		"node3: no KVM",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not say %q: %s", want, msg)
		}
	}
	if node.created() != 0 || localHas(h, "vm1") {
		t.Fatal("a machine exists after a refused create")
	}
}

func TestCreateOnANamedNode(t *testing.T) {
	// The named node takes the machine even when another node has more room.
	small := roomy
	small.MemoryFreeMB = 3000
	node := newFakeNode(t, small)
	h := placementHarness(t, roomy, map[string]*fakeNode{"node2": node})

	doc := machineDoc("vm1")
	doc["node"] = "node2"
	if rec := h.do("POST", "/api/machines", "alice", doc); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if node.created() != 1 {
		t.Fatal("the named node did not get the machine")
	}

	doc = machineDoc("vm2")
	doc["node"] = "local"
	if rec := h.do("POST", "/api/machines", "alice", doc); rec.Code != http.StatusCreated {
		t.Fatalf("local: status = %d: %s", rec.Code, rec.Body)
	}
	if node.created() != 1 {
		t.Fatal("a create for this node went to a peer")
	}

	doc = machineDoc("vm3")
	doc["node"] = "nowhere"
	if rec := h.do("POST", "/api/machines", "alice", doc); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown node: status = %d, want 400", rec.Code)
	}
}

// A create that a node forwards must not be placed a second time, or two
// nodes send it to each other.
func TestForwardedCreateIsNotPlacedAgain(t *testing.T) {
	small := roomy
	small.MemoryFreeMB = 3000
	bigger := newFakeNode(t, roomy)
	h := placementHarness(t, small, map[string]*fakeNode{"node2": bigger})

	rec := h.withToken("POST", "/api/machines", "secret", machineDoc("vm1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if bigger.created() != 0 {
		t.Fatal("a forwarded create was forwarded again")
	}
}

func TestBadDocumentIsRefusedBeforePlacement(t *testing.T) {
	node := newFakeNode(t, roomy)
	h := placementHarness(t, roomy, map[string]*fakeNode{"node2": node})
	rec := h.do("POST", "/api/machines", "alice", map[string]any{"id": "bad id!", "memoryMb": 1})
	if rec.Code != http.StatusBadRequest || node.created() != 0 {
		t.Fatalf("status = %d, creates = %d", rec.Code, node.created())
	}
}

func TestCapacityEndpoint(t *testing.T) {
	h := newHarness(t)
	h.srv.capacity = func() peer.Capacity { return roomy }
	got := decode[peer.Capacity](t, h.do("GET", "/api/cluster/capacity", "", nil))
	if got != roomy {
		t.Fatalf("capacity = %+v", got)
	}
}

// A lab with one node and no named node creates as before: no capacity
// check stands in the way of a laptop with no KVM.
func TestSingleNodeCreateHasNoPlacement(t *testing.T) {
	h := newHarness(t)
	enableLifecycle(t, h)
	h.srv.capacity = func() peer.Capacity { return peer.Capacity{} }
	if rec := h.do("POST", "/api/machines", "alice", machineDoc("vm1")); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}
