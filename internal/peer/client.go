package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds one call to a peer. The wall polls the machine
// list, so a slow peer must not hold every other tile.
const DefaultTimeout = 2 * time.Second

// Client reaches the configured peers.
type Client struct {
	peers          []Peer
	byName         map[string]Peer
	http           *http.Client
	identityHeader string
	clusterToken   string
}

// Options configures a Client.
type Options struct {
	Peers          []Peer
	IdentityHeader string
	ClusterToken   string
	Timeout        time.Duration
	Transport      http.RoundTripper
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
	return &Client{
		peers:          append([]Peer(nil), opts.Peers...),
		byName:         byName,
		identityHeader: header,
		clusterToken:   opts.ClusterToken,
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
	return len(c.peers)
}

// Names returns the configured peer names in order.
func (c *Client) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, len(c.peers))
	for i, p := range c.peers {
		out[i] = p.Name
	}
	return out
}

// Lookup finds a peer by name.
func (c *Client) Lookup(name string) (Peer, bool) {
	if c == nil {
		return Peer{}, false
	}
	p, ok := c.byName[name]
	return p, ok
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
	if c == nil || len(c.peers) == 0 {
		return nil, nil
	}

	type result struct {
		peer     string
		machines []map[string]any
		err      error
	}
	ch := make(chan result, len(c.peers))
	var wg sync.WaitGroup
	for _, p := range c.peers {
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
