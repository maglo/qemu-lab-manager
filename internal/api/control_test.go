package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

// fakeQEMU answers QMP on a pipe, so the API tests exercise the real client
// without a hypervisor. The harness dials it for every machine.
type fakeQEMU struct {
	t   *testing.T
	png []byte

	mu      sync.Mutex
	refuse  bool
	keys    [][]string
	touched bool
}

func newFakeQEMU(t *testing.T) *fakeQEMU {
	return &fakeQEMU{t: t, png: []byte("\x89PNG\r\n\x1a\nfake frame")}
}

// refuseDial makes the control socket unreachable, as a machine that is
// switched off would be.
func (f *fakeQEMU) refuseDial(refuse bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = refuse
}

func (f *fakeQEMU) sent() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys
}

func (f *fakeQEMU) dial(ctx context.Context, network, address string) (net.Conn, error) {
	f.mu.Lock()
	refuse := f.refuse
	f.mu.Unlock()
	if refuse {
		// The address is in the error, which is exactly what must not reach
		// a client.
		return nil, &net.OpError{Op: "dial", Net: network,
			Addr: &net.UnixAddr{Name: address, Net: network}, Err: os.ErrNotExist}
	}

	ours, theirs := net.Pipe()
	go f.serve(theirs)
	return ours, nil
}

func (f *fakeQEMU) serve(conn net.Conn) {
	defer conn.Close()
	write := func(v any) bool {
		b, _ := json.Marshal(v)
		_, err := conn.Write(append(b, '\n'))
		return err == nil
	}
	if !write(map[string]any{"QMP": map[string]any{"capabilities": []string{}}}) {
		return
	}

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var req struct {
			Execute   string         `json:"execute"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			return
		}
		switch req.Execute {
		case "qmp_capabilities":
			write(map[string]any{"return": map[string]any{}})
		case "send-key":
			var keys []string
			list, _ := req.Arguments["keys"].([]any)
			for _, k := range list {
				entry, _ := k.(map[string]any)
				data, _ := entry["data"].(string)
				keys = append(keys, data)
			}
			f.mu.Lock()
			f.keys = append(f.keys, keys)
			f.mu.Unlock()
			write(map[string]any{"return": map[string]any{}})
		case "screendump":
			path, _ := req.Arguments["filename"].(string)
			if err := os.WriteFile(path, f.png, 0o644); err != nil {
				write(map[string]any{"error": map[string]string{
					"class": "GenericError", "desc": err.Error()}})
				continue
			}
			write(map[string]any{"return": map[string]any{}})
		default:
			write(map[string]any{"error": map[string]string{
				"class": "CommandNotFound", "desc": "unknown command"}})
		}
	}
}

// The keyboard is the keyboard whichever way it is reached, so the endpoint
// takes the lease that gates the framebuffer and the serial line.
func TestKeysNeedTheWriteLease(t *testing.T) {
	h := newHarness(t)
	chord := keysRequest{Keys: []string{"ctrl", "alt", "f3"}}

	rec := h.do("POST", "/api/machines/el9-build/keys", "alice", chord)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status without a lease = %d, want 409: %s", rec.Code, rec.Body)
	}
	if len(h.qemu.sent()) != 0 {
		t.Fatal("a client with no lease reached the machine")
	}

	if rec := h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "acquire"}); rec.Code != http.StatusOK {
		t.Fatalf("acquire: %d", rec.Code)
	}
	if rec := h.do("POST", "/api/machines/el9-build/keys", "alice", chord); rec.Code != http.StatusOK {
		t.Fatalf("status with the lease = %d: %s", rec.Code, rec.Body)
	}
	sent := h.qemu.sent()
	if len(sent) != 1 || strings.Join(sent[0], "+") != "ctrl+alt+f3" {
		t.Fatalf("the machine received %v, want one chord", sent)
	}

	// Somebody else is driving, and the refusal says who.
	rec = h.do("POST", "/api/machines/el9-build/keys", "bob", chord)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status for the second user = %d, want 409", rec.Code)
	}
	body := decode[errorBody](t, rec)
	if body.Holder != "alice" {
		t.Errorf("holder = %q, want alice", body.Holder)
	}
}

// A chord is input, and input is what keeps a lease alive.
func TestKeysExtendTheLease(t *testing.T) {
	h := newHarness(t)
	if rec := h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "acquire"}); rec.Code != http.StatusOK {
		t.Fatalf("acquire: %d", rec.Code)
	}
	before, _ := h.leases.Get("el9-build")

	if rec := h.do("POST", "/api/machines/el9-build/keys", "alice", keysRequest{Keys: []string{"ret"}}); rec.Code != http.StatusOK {
		t.Fatalf("keys: %d: %s", rec.Code, rec.Body)
	}
	after, _ := h.leases.Get("el9-build")
	if !after.Expires.After(before.Expires) {
		t.Errorf("expiry %s did not move past %s", after.Expires, before.Expires)
	}
}

func TestKeysAreValidatedBeforeTheMachineIsReached(t *testing.T) {
	h := newHarness(t)
	if rec := h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "acquire"}); rec.Code != http.StatusOK {
		t.Fatalf("acquire: %d", rec.Code)
	}
	for _, chord := range []keysRequest{
		{Keys: nil},
		{Keys: []string{"ctrl", "alt; reboot"}},
	} {
		rec := h.do("POST", "/api/machines/el9-build/keys", "alice", chord)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want 400", chord.Keys, rec.Code)
		}
	}
	if len(h.qemu.sent()) != 0 {
		t.Error("an invalid chord reached the machine")
	}
}

// A machine with no control socket says it has none. That is not a failure,
// and it is not a 404 either: the machine is there, the channel is not.
func TestMachineWithNoControlSocket(t *testing.T) {
	h := newHarness(t)
	if rec := h.do("POST", "/api/machines/no-unit/lease", "alice", leaseRequest{Action: "acquire"}); rec.Code != http.StatusOK {
		t.Fatalf("acquire: %d", rec.Code)
	}

	rec := h.do("POST", "/api/machines/no-unit/keys", "alice", keysRequest{Keys: []string{"ret"}})
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("keys status = %d, want 501: %s", rec.Code, rec.Body)
	}
	rec = h.do("GET", "/api/machines/no-unit/screenshot", "alice", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("screenshot status = %d, want 501: %s", rec.Code, rec.Body)
	}

	// And the listing says so, so a tile knows before it asks.
	rec = h.do("GET", "/api/machines", "alice", nil)
	var body struct {
		Machines []struct {
			ID         string `json:"id"`
			HasControl bool   `json:"hasControl"`
		} `json:"machines"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	for _, m := range body.Machines {
		if want := m.ID == "el9-build"; m.HasControl != want {
			t.Errorf("%s hasControl = %v, want %v", m.ID, m.HasControl, want)
		}
	}
}

// The wall is view-only for everyone and shows every machine's screen
// already, so a capture of that screen is a read, and read is free.
func TestScreenshotNeedsNoLease(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/api/machines/el9-build/screenshot", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("content type = %q", got)
	}
	if !strings.HasPrefix(rec.Body.String(), "\x89PNG") {
		t.Errorf("body is not the PNG QEMU wrote: %q", rec.Body.String()[:8])
	}

	// The capture is a frame of somebody's screen, so it does not stay on
	// disk.
	left, err := os.ReadDir(h.shotDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the capture directory still holds %d file(s)", len(left))
	}
}

// A control socket that does not answer names itself in the error, and
// section 6 keeps every address off every client.
func TestControlFailureWithholdsTheSocketPath(t *testing.T) {
	h := newHarness(t)
	h.qemu.refuseDial(true)
	if rec := h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "acquire"}); rec.Code != http.StatusOK {
		t.Fatalf("acquire: %d", rec.Code)
	}

	for _, call := range []struct{ method, path string }{
		{"POST", "/api/machines/el9-build/keys"},
		{"GET", "/api/machines/el9-build/screenshot"},
	} {
		var body any
		if call.method == "POST" {
			body = keysRequest{Keys: []string{"ret"}}
		}
		rec := h.do(call.method, call.path, "alice", body)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s: status = %d, want 502", call.path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "/run/qemu") {
			t.Errorf("%s leaked the socket path: %s", call.path, rec.Body)
		}
	}
}
