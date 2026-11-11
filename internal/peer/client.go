package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds one call to a peer. The wall polls the machine
// list, so a slow peer must not hold every other tile.
const DefaultTimeout = 2 * time.Second

// DefaultNodeTTL is how long a registration lasts with no renewal.
const DefaultNodeTTL = 90 * time.Second

// Client reaches the peers. A peer comes from a -peer flag, or it
// registers at run time (docs/design/cluster.md section 5).
type Client struct {
	peers          []Peer
	byName         map[string]Peer
	http           *http.Client
	identityHeader string
	clusterToken   string
	ttl            time.Duration
	now            func() time.Time

	mu     sync.Mutex
	joined map[string]joinedNode
}

type joinedNode struct {
	peer     Peer
	kvm      bool
	lastSeen time.Time
}

// Options configures a Client.
type Options struct {
	Peers          []Peer
	IdentityHeader string
	ClusterToken   string
	Timeout        time.Duration
	Transport      http.RoundTripper
	// NodeTTL is how long a registration lasts with no renewal.
	NodeTTL time.Duration
	// Now is the clock. Tests step it.
	Now func() time.Time
}

// New returns a Client for the given peers. An empty peer list is valid.
func New(opts Options) (*Client, error) {
	byName := make(map[string]Peer, len(opts.Peers))
	for _, p := range opts.Peers {
		if err := validateName(p.Name); err != nil {
			return nil, err
		}
		if _, exists := byName[p.Name]; exists {
			return nil, fmt.Errorf("peer name %q is repeated", p.Name)
		}
		byName[p.Name] = p
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	header := opts.IdentityHeader
	if header == "" {
		header = "X-Forwarded-User"
	}
	transport := opts.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	ttl := opts.NodeTTL
	if ttl <= 0 {
		ttl = DefaultNodeTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		peers:          append([]Peer(nil), opts.Peers...),
		byName:         byName,
		identityHeader: header,
		clusterToken:   opts.ClusterToken,
		ttl:            ttl,
		now:            now,
		joined:         make(map[string]joinedNode),
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}, nil
}

// Len reports how many peers the client holds.
func (c *Client) Len() int {
	if c == nil {
		return 0
	}
	return len(c.current())
}

// Names returns the peer names. Flag peers come first, in flag order.
func (c *Client) Names() []string {
	if c == nil {
		return nil
	}
	peers := c.current()
	out := make([]string, len(peers))
	for i, p := range peers {
		out[i] = p.Name
	}
	return out
}

// Lookup finds a peer by name.
func (c *Client) Lookup(name string) (Peer, bool) {
	if c == nil {
		return Peer{}, false
	}
	if p, ok := c.byName[name]; ok {
		return p, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.joined[name]
	if !ok || c.expired(n) {
		return Peer{}, false
	}
	return n.peer, true
}

// current returns the flag peers and the registered nodes that are live.
func (c *Client) current() []Peer {
	out := append([]Peer(nil), c.peers...)
	c.mu.Lock()
	defer c.mu.Unlock()
	first := len(out)
	for name, n := range c.joined {
		if c.expired(n) {
			delete(c.joined, name)
			continue
		}
		out = append(out, n.peer)
	}
	sort.Slice(out[first:], func(i, j int) bool { return out[first+i].Name < out[first+j].Name })
	return out
}

func (c *Client) expired(n joinedNode) bool {
	return c.now().Sub(n.lastSeen) > c.ttl
}

// ErrFlagPeer says that a registration names a peer of a -peer flag.
var ErrFlagPeer = errors.New("the name belongs to a -peer flag")

// Register adds a node, or renews it. isNew is true for a node that was
// not live before the call.
func (c *Client) Register(r Registration) (isNew bool, err error) {
	p, err := ParseFlag(r.Name + "=" + r.URL)
	if err != nil {
		return false, err
	}
	if _, ok := c.byName[p.Name]; ok {
		return false, fmt.Errorf("node %q: %w", p.Name, ErrFlagPeer)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.joined[p.Name]
	isNew = !ok || c.expired(old)
	c.joined[p.Name] = joinedNode{peer: p, kvm: r.KVM, lastSeen: c.now()}
	return isNew, nil
}

// TTL is how long a registration lasts with no renewal.
func (c *Client) TTL() time.Duration { return c.ttl }

// Node is one peer as GET /api/cluster/nodes shows it. It holds no URL,
// because a client never needs the address of a node.
type Node struct {
	Name string `json:"name"`
	// Source is "flag" or "join".
	Source string `json:"source"`
	// KVM is nil for a flag peer, which does not report it.
	KVM *bool `json:"kvm,omitempty"`
	// Ready is false for a node that reports no KVM.
	Ready    bool       `json:"ready"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// Nodes lists the flag peers and the registered nodes that are live.
func (c *Client) Nodes() []Node {
	if c == nil {
		return nil
	}
	out := make([]Node, 0, len(c.peers))
	for _, p := range c.peers {
		out = append(out, Node{Name: p.Name, Source: "flag", Ready: true})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	first := len(out)
	for name, n := range c.joined {
		if c.expired(n) {
			delete(c.joined, name)
			continue
		}
		kvm, seen := n.kvm, n.lastSeen
		out = append(out, Node{Name: name, Source: "join", KVM: &kvm, Ready: kvm, LastSeen: &seen})
	}
	sort.Slice(out[first:], func(i, j int) bool { return out[first+i].Name < out[first+j].Name })
	return out
}

// Failure is one peer that did not answer a list call.
type Failure struct {
	Peer  string `json:"peer"`
	Error string `json:"error"`
}

// ListMachines pulls GET /api/machines from every peer and rewrites each
// machine id to the composite form. A peer that fails is named in failed
// and does not block the others.
func (c *Client) ListMachines(ctx context.Context, identity string) (machines []map[string]any, failed []Failure) {
	if c == nil {
		return nil, nil
	}
	peers := c.current()
	if len(peers) == 0 {
		return nil, nil
	}

	type result struct {
		peer     string
		machines []map[string]any
		err      error
	}
	ch := make(chan result, len(peers))
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			ms, err := c.listOne(ctx, p, identity)
			ch <- result{peer: p.Name, machines: ms, err: err}
		}(p)
	}
	wg.Wait()
	close(ch)

	for r := range ch {
		if r.err != nil {
			failed = append(failed, Failure{Peer: r.peer, Error: r.err.Error()})
			continue
		}
		machines = append(machines, r.machines...)
	}
	return machines, failed
}

func (c *Client) listOne(ctx context.Context, p Peer, identity string) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+"/api/machines", nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, identity)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", res.StatusCode, truncate(string(body), 200))
	}
	var parsed struct {
		Machines []map[string]any `json:"machines"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make([]map[string]any, 0, len(parsed.Machines))
	for _, m := range parsed.Machines {
		localID, _ := m["id"].(string)
		if localID == "" {
			continue
		}
		m["id"] = JoinID(p.Name, localID)
		if host, _ := m["host"].(string); host == "" {
			m["host"] = p.Name
		}
		out = append(out, m)
	}
	return out, nil
}

// ProxyMachine forwards a machine-scoped HTTP request to the peer that
// owns it. The composite id in the path becomes the peer's local id.
func (c *Client) ProxyMachine(w http.ResponseWriter, r *http.Request, identity string) bool {
	if c == nil {
		return false
	}
	id := r.PathValue("id")
	name, localID, ok := SplitID(id)
	if !ok {
		return false
	}
	p, ok := c.Lookup(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "no peer %q", name)
		return true
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeJSONError(w, http.StatusNotImplemented,
			"websocket forwarding to peer %q is not available yet", name)
		return true
	}

	target, err := url.Parse(p.URL)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "peer %q has a bad URL", name)
		return true
	}

	token := c.clusterToken
	identityHeader := c.identityHeader
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = rewriteMachinePath(req.URL.Path, id, localID)
			req.URL.RawPath = ""
			if identity != "" {
				req.Header.Set(identityHeader, identity)
			}
			if token != "" {
				req.Header.Set(ClusterTokenHeader, token)
			}
			req.Header.Del("Accept-Encoding")
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
			writeJSONError(rw, http.StatusBadGateway,
				"peer %q did not answer: %s", name, err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
	return true
}

func (c *Client) setHeaders(req *http.Request, identity string) {
	if identity != "" {
		req.Header.Set(c.identityHeader, identity)
	}
	if c.clusterToken != "" {
		req.Header.Set(ClusterTokenHeader, c.clusterToken)
	}
}

func rewriteMachinePath(path, compositeID, localID string) string {
	for _, root := range []string{"/api/machines/", "/ws/console/", "/ws/serial/"} {
		prefix := root + compositeID
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return root + localID + strings.TrimPrefix(path, prefix)
		}
	}
	return path
}

func writeJSONError(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "{\"error\":%q}\n", fmt.Sprintf(format, args...))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
