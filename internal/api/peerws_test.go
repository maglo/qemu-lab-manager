package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
)

// frontOf is an API node that has the given node as its peer "node2".
func frontOf(t *testing.T, nodeURL string) (*harness, *httptest.Server) {
	t.Helper()
	h := newHarness(t)
	client, err := peer.New(peer.Options{
		Peers:          []peer.Peer{{Name: "node2", URL: nodeURL}},
		IdentityHeader: h.srv.cfg.IdentityHeader,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.peers = client
	ts := httptest.NewServer(h.srv)
	t.Cleanup(ts.Close)
	return h, ts
}

func dialVia(t *testing.T, ts *httptest.Server, path, user string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	if header == nil {
		header = http.Header{}
	}
	header.Set("X-Forwarded-User", user)
	c, resp, err := websocket.Dial(context.Background(),
		"ws"+strings.TrimPrefix(ts.URL, "http")+path, &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		c.SetReadLimit(1 << 20)
		t.Cleanup(func() { c.CloseNow() })
	}
	return c, resp, err
}

// The serial line of a machine on a peer works through the API node: output
// comes back, input reaches the machine, and the lease is on the peer.
func TestSerialWSForwardsToThePeer(t *testing.T) {
	owner := newWSHarness(t)
	front, ts := frontOf(t, owner.ts.URL)

	owner.vm.print("before attach\r\n")
	conn, _, err := dialVia(t, ts, "/ws/serial/node2:vm1?write=1", "alice", nil)
	if err != nil {
		t.Fatalf("dial through the API node: %v", err)
	}
	c := newClient(t, conn)

	c.waitOutput(t, "before attach", 3*time.Second)
	waitUntil(t, 3*time.Second, func() bool { return owner.leases.CanWrite("vm1", "alice") },
		"the node that owns the machine did not give alice the lease")
	if front.srv.leases.CanWrite("vm1", "alice") || front.srv.leases.CanWrite("node2:vm1", "alice") {
		t.Error("the API node holds a lease for a machine of a peer")
	}

	c.send(t, websocket.MessageBinary, []byte("uname\n"))
	waitUntil(t, 3*time.Second, func() bool { return strings.Contains(owner.vm.input(), "uname") },
		"input did not reach the machine")

	owner.vm.print("Linux\r\n")
	c.waitOutput(t, "Linux", 3*time.Second)
}

// A browser that closes the socket on the API node releases the lease on
// the peer. A lease that stays is a machine that nobody can drive.
func TestClosingTheForwardedSocketReleasesTheLease(t *testing.T) {
	owner := newWSHarness(t)
	_, ts := frontOf(t, owner.ts.URL)

	conn, _, err := dialVia(t, ts, "/ws/serial/node2:vm1?write=1", "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	newClient(t, conn)
	waitUntil(t, 3*time.Second, func() bool { return owner.leases.CanWrite("vm1", "alice") },
		"alice did not get the lease")

	conn.Close(websocket.StatusNormalClosure, "done")
	waitUntil(t, 3*time.Second, func() bool { return !owner.leases.CanWrite("vm1", "alice") },
		"the lease on the peer outlived the socket on the API node")
}

// The peer closes its side when the machine has no serial line there. The
// client of the API node must see the refusal, and not a socket that hangs.
func TestForwardedSocketReportsThePeerRefusal(t *testing.T) {
	owner := newWSHarness(t)
	_, ts := frontOf(t, owner.ts.URL)

	_, resp, err := dialVia(t, ts, "/ws/serial/node2:absent", "alice", nil)
	if err == nil {
		t.Fatal("a socket opened for a machine that the peer does not have")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("response = %v, want the 404 of the peer", resp)
	}
}

func TestForwardedSocketChecksTheOrigin(t *testing.T) {
	owner := newWSHarness(t)
	_, ts := frontOf(t, owner.ts.URL)
	frontHost := strings.TrimPrefix(ts.URL, "http://")

	_, resp, err := dialVia(t, ts, "/ws/serial/node2:vm1", "alice",
		http.Header{"Origin": []string{"http://evil.example"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a page of another site opened a socket: err=%v resp=%v", err, resp)
	}

	// The page of the API node names the host of the API node. The peer
	// has another host, and the socket must still open.
	conn, _, err := dialVia(t, ts, "/ws/serial/node2:vm1", "alice",
		http.Header{"Origin": []string{"http://" + frontHost}})
	if err != nil {
		t.Fatalf("the page of the API node could not open a socket: %v", err)
	}
	owner.vm.print("hello\r\n")
	newClient(t, conn).waitOutput(t, "hello", 3*time.Second)
}

// The console path forwards as the serial path does. The peer here is a
// plain echo, because the fake VM has no framebuffer.
func TestConsoleWSForwardsToThePeer(t *testing.T) {
	var mu sync.Mutex
	var sawPath, sawUser, sawToken, sawOrigin string
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawPath, sawUser = r.URL.Path, r.Header.Get("X-Forwarded-User")
		sawToken, sawOrigin = r.Header.Get(peer.ClusterTokenHeader), r.Header.Get("Origin")
		mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			kind, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), kind, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(echo.Close)

	h := newHarness(t)
	h.srv.cfg.ClusterToken = "secret"
	client, err := peer.New(peer.Options{
		Peers:        []peer.Peer{{Name: "node2", URL: echo.URL}},
		ClusterToken: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.peers = client
	ts := httptest.NewServer(h.srv)
	t.Cleanup(ts.Close)

	conn, _, err := dialVia(t, ts, "/ws/console/node2:vm1", "alice",
		http.Header{"Origin": []string{ts.URL}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	kind, data, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || string(data) != "RFB 003.008\n" {
		t.Fatalf("echo = %v %q %v", kind, data, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if sawPath != "/ws/console/vm1" || sawUser != "alice" || sawToken != "secret" {
		t.Errorf("the peer saw path=%q user=%q token=%q", sawPath, sawUser, sawToken)
	}
	if sawOrigin != "" {
		t.Errorf("the peer saw the origin %q of the API node", sawOrigin)
	}
}

func TestForwardedSocketToADeadPeer(t *testing.T) {
	_, ts := frontOf(t, "http://127.0.0.1:1")
	_, resp, err := dialVia(t, ts, "/ws/serial/node2:vm1", "alice", nil)
	if err == nil {
		t.Fatal("a socket opened to a peer that is not there")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("response = %v %s, want 502", resp, body)
	}
}
