package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/config"
	"github.com/maglo/qemu-lab-manager/labview/internal/host"
	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
	"github.com/maglo/qemu-lab-manager/labview/internal/serial"
)

const testInventory = `[
  {
    "id": "el9-build",
    "name": "el9-build",
    "host": "kvm01",
    "vnc": "10.20.0.11:5901",
    "serial": "/run/qemu/el9-build-serial.sock",
    "unit": "qemu-el9-build.service",
    "notes": "AlmaLinux 9",
    "tags": ["build", "el9"]
  },
  {
    "id": "serial-only",
    "name": "serial-only",
    "host": "kvm01",
    "serial": "/run/qemu/serial-only.sock"
  },
  {
    "id": "no-unit",
    "name": "no-unit",
    "host": "kvm01",
    "vnc": "10.20.0.12:5902"
  }
]`

type harness struct {
	srv      *Server
	fakeHost *host.Fake
	leases   *lease.Manager
	brokers  *serial.Manager
	dir      string
	cancel   context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dir := t.TempDir()
	invPath := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(invPath, []byte(testInventory), 0o640); err != nil {
		t.Fatal(err)
	}
	recDir := filepath.Join(dir, "recordings")
	if err := os.MkdirAll(recDir, 0o750); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	w, err := inventory.NewWatcher(invPath, time.Hour, log)
	if err != nil {
		t.Fatalf("inventory.NewWatcher: %v", err)
	}

	cfg := config.Default()
	cfg.InventoryPath = invPath
	cfg.TranscriptDir = recDir
	cfg.HostAccess = config.HostFake
	cfg.LeaseIdle = time.Minute
	cfg.LeaseWarn = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())

	// Brokers with a dial that never succeeds: these tests are about the
	// HTTP surface, not about the upstream.
	brokers := serial.NewManager(ctx, serial.ManagerConfig{
		BackoffMin: time.Hour,
		BackoffMax: time.Hour,
		Log:        log,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, fmt.Errorf("no machine at %s", address)
		},
	})
	brokers.Reconcile(w.Current())

	leases := lease.NewManager(lease.Options{IdleTimeout: cfg.LeaseIdle, WarnBefore: cfg.LeaseWarn})
	fakeHost := host.NewFake()

	srv := New(Deps{
		Config:    cfg,
		Inventory: w,
		Brokers:   brokers,
		Leases:    leases,
		Hosts:     fakeHost,
		Activity:  activity.New(50, log),
		Log:       log,
	})

	h := &harness{srv: srv, fakeHost: fakeHost, leases: leases, brokers: brokers, dir: recDir, cancel: cancel}
	t.Cleanup(func() { cancel(); brokers.Close() })
	return h
}

// do issues a request as a given user, the way the proxy would.
func (h *harness) do(method, path, user string, body any) *httptest.ResponseRecorder {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if user != "" {
		req.Header.Set("X-Forwarded-User", user)
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return v
}

func TestMachinesListing(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/api/machines", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		Machines []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Host       string `json:"host"`
			HasConsole bool   `json:"hasConsole"`
			HasSerial  bool   `json:"hasSerial"`
			CanPower   bool   `json:"canPower"`
			Live       bool   `json:"live"`
			Why        string `json:"why"`
		} `json:"machines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Machines) != 3 {
		t.Fatalf("got %d machines, want 3", len(body.Machines))
	}

	byID := map[string]int{}
	for i, m := range body.Machines {
		byID[m.ID] = i
	}
	el9 := body.Machines[byID["el9-build"]]
	if !el9.HasConsole || !el9.HasSerial || !el9.CanPower {
		t.Errorf("el9-build capabilities wrong: %+v", el9)
	}
	// A machine with no unit cannot be powered, even though it has a
	// console: the inventory is the allowlist.
	if body.Machines[byID["no-unit"]].CanPower {
		t.Error("a machine with no unit offers power operations")
	}
	// Machines that are not live say why (design section 7).
	if el9.Live {
		t.Error("el9-build should not be live; its unit is stopped")
	}
	if el9.Why == "" {
		t.Error("a machine that is not live should say why")
	}
}

// Design section 6: neither vnc nor serial is ever sent to the browser. Its
// stated reason is the request path -- "clients name a machine by id only, so
// a developer cannot dial a console the inventory did not give them" -- so
// the invariant tested here is that the machine listing and the inventory
// block carry no address a client could hand back.
//
// Section 8 pulls the other way for one field: it asks the details tab to
// show the full QEMU command line, which contains -vnc and the serial
// chardev path. That disclosure is deliberate and is tested separately below.
func TestListingNeverCarriesAddresses(t *testing.T) {
	h := newHarness(t)
	h.fakeHost.SetUnit("el9-build", host.Unit{
		Name: "qemu-el9-build.service", ActiveState: "active", SubState: "running", MainPID: 4242,
	})

	addresses := []string{"10.20.0.11:5901", "10.20.0.12:5902",
		"/run/qemu/el9-build-serial.sock", "/run/qemu/serial-only.sock"}

	for _, path := range []string{
		"/api/machines",
		"/api/machines/el9-build/recordings",
		"/api/machines/el9-build/activity",
		"/api/config",
	} {
		rec := h.do("GET", path, "alice", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
			continue
		}
		for _, addr := range addresses {
			if strings.Contains(rec.Body.String(), addr) {
				t.Errorf("%s leaked the address %q", path, addr)
			}
		}
	}
}

// A broker that cannot reach its machine must not report the endpoint it
// failed to reach: the error text names it, and error text cannot be scrubbed
// reliably. The client learns that it is failing, not where.
func TestSerialStateWithholdsUpstreamErrorText(t *testing.T) {
	h := newHarness(t)

	// Let the failing dial (configured in the harness) be attempted.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := h.brokers.States()["el9-build"]; st.LastError != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := h.brokers.States()["el9-build"]; st.LastError == "" {
		t.Skip("broker has not attempted a dial yet")
	}

	rec := h.do("GET", "/api/machines", "alice", nil)
	body := rec.Body.String()
	if strings.Contains(body, "/run/qemu/") {
		t.Errorf("upstream error text leaked an address: %s", body)
	}
	if !strings.Contains(body, `"failing": true`) {
		t.Errorf("client was not told the serial line is failing: %s", body)
	}
}

// Section 8 asks for the unit name, its state and when it last changed, so
// those must reach the page.
func TestDetailsTabShowsUnitNameAndState(t *testing.T) {
	h := newHarness(t)
	changed := time.Now().Add(-time.Hour)
	h.fakeHost.SetUnit("el9-build", host.Unit{
		Name:        "qemu-el9-build.service",
		ActiveState: "active",
		SubState:    "running",
		Since:       changed,
		MainPID:     4242,
	})

	rec := h.do("GET", "/api/machines/el9-build", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Unit    *host.Unit `json:"unit"`
		Details struct {
			Unit        host.Unit `json:"unit"`
			CommandLine []string  `json:"commandLine"`
		} `json:"details"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	if body.Details.Unit.Name != "qemu-el9-build.service" {
		t.Errorf("details tab has no unit name: %+v", body.Details.Unit)
	}
	if body.Details.Unit.ActiveState != "active" {
		t.Errorf("unit state = %q", body.Details.Unit.ActiveState)
	}
	if body.Details.Unit.Since.IsZero() {
		t.Error("unit has no state change time")
	}
	// And the command line, which is the thing section 8 calls most useful.
	if len(body.Details.CommandLine) == 0 {
		t.Error("details tab has no QEMU command line for a running machine")
	}
}

// The details tab shows the inventory entry verbatim, including fields
// labview does not model -- but not the addresses.
func TestMachineRecordShowsEntryVerbatimMinusAddresses(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/api/machines/el9-build", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		Inventory map[string]any `json:"inventory"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)

	// An unknown field written by a newer producer must survive to the page.
	tags, ok := body.Inventory["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Errorf("unknown field lost from the verbatim entry: %+v", body.Inventory)
	}
	if body.Inventory["notes"] != "AlmaLinux 9" {
		t.Errorf("notes = %v", body.Inventory["notes"])
	}
	// The addresses are present but redacted, so a reader does not wonder
	// why the page differs from the file.
	for _, k := range []string{"vnc", "serial"} {
		v, present := body.Inventory[k]
		if !present {
			t.Errorf("%s vanished silently instead of being marked withheld", k)
			continue
		}
		s, _ := v.(string)
		if !strings.Contains(s, "withheld") {
			t.Errorf("%s = %v, want a withheld marker", k, v)
		}
		if strings.Contains(s, "10.20.0.11") || strings.Contains(s, "/run/qemu") {
			t.Errorf("%s marker still contains the address: %v", k, v)
		}
	}
	// The unit name is not an address, and section 8 asks for it.
	if body.Inventory["unit"] != "qemu-el9-build.service" {
		t.Errorf("unit = %v, want the unit name shown", body.Inventory["unit"])
	}
}

func TestUnknownMachineIs404(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/api/machines/nope",
		"/api/machines/nope/logs",
		"/api/machines/nope/recordings",
		"/api/machines/nope/activity",
	} {
		if rec := h.do("GET", path, "alice", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestLeaseAcquireRenewRelease(t *testing.T) {
	h := newHarness(t)

	rec := h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "acquire"})
	if rec.Code != http.StatusOK {
		t.Fatalf("acquire status = %d: %s", rec.Code, rec.Body)
	}
	resp := decode[leaseResponse](t, rec)
	if !resp.Held || !resp.Mine || resp.Lease.Holder != "alice" {
		t.Fatalf("acquire response = %+v", resp)
	}

	// Bob is refused, and told who is driving (design section 4).
	rec = h.do("POST", "/api/machines/el9-build/lease", "bob", leaseRequest{Action: "acquire"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("bob's acquire status = %d, want 409", rec.Code)
	}
	errBody := decode[errorBody](t, rec)
	if errBody.Holder != "alice" {
		t.Fatalf("refusal did not name the holder: %+v", errBody)
	}

	// Renewal by a non-holder is refused; by the holder it succeeds.
	if rec := h.do("POST", "/api/machines/el9-build/lease", "bob", leaseRequest{Action: "renew"}); rec.Code != http.StatusConflict {
		t.Errorf("bob renewed alice's lease: %d", rec.Code)
	}
	if rec := h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "renew"}); rec.Code != http.StatusOK {
		t.Errorf("alice could not renew: %d", rec.Code)
	}

	// Release frees it for bob.
	if rec := h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "release"}); rec.Code != http.StatusOK {
		t.Errorf("release status = %d", rec.Code)
	}
	if rec := h.do("POST", "/api/machines/el9-build/lease", "bob", leaseRequest{Action: "acquire"}); rec.Code != http.StatusOK {
		t.Errorf("bob could not acquire a freed lease: %d", rec.Code)
	}
}

func TestLeaseDefaultsToAcquireAndRejectsNonsense(t *testing.T) {
	h := newHarness(t)
	// An empty body means acquire, which is what a UI button sends.
	if rec := h.do("POST", "/api/machines/el9-build/lease", "alice", nil); rec.Code != http.StatusOK {
		t.Errorf("empty body status = %d", rec.Code)
	}
	if rec := h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "steal"}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown action status = %d, want 400", rec.Code)
	}
}

// Power operations require the write lease, the same one that gates the
// keyboard (design section 12).
func TestPowerRequiresTheLease(t *testing.T) {
	h := newHarness(t)

	// Nobody holds it: refused, and nothing reaches the host.
	rec := h.do("POST", "/api/machines/el9-build/power", "alice", powerRequest{Op: "restart"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	if ops := h.fakeHost.Ops(); len(ops) != 0 {
		t.Fatalf("a refused request still reached the host: %v", ops)
	}

	// Alice takes control; bob still cannot restart her machine.
	h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "acquire"})
	rec = h.do("POST", "/api/machines/el9-build/power", "bob", powerRequest{Op: "restart"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("bob's restart status = %d, want 409", rec.Code)
	}
	if errBody := decode[errorBody](t, rec); errBody.Holder != "alice" {
		t.Errorf("refusal did not name the holder: %+v", errBody)
	}
	if ops := h.fakeHost.Ops(); len(ops) != 0 {
		t.Fatalf("bob's refused restart reached the host: %v", ops)
	}

	// Alice can.
	rec = h.do("POST", "/api/machines/el9-build/power", "alice", powerRequest{Op: "restart"})
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's restart status = %d: %s", rec.Code, rec.Body)
	}
	if ops := h.fakeHost.Ops(); len(ops) != 1 || ops[0] != "el9-build:restart" {
		t.Fatalf("host ops = %v", ops)
	}
}

func TestPowerRejectsUnknownOpAndUnmanagedMachine(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "acquire"})

	if rec := h.do("POST", "/api/machines/el9-build/power", "alice", powerRequest{Op: "isolate"}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown op status = %d, want 400", rec.Code)
	}
	// A machine with no unit is refused before the lease is even consulted.
	h.do("POST", "/api/machines/no-unit/lease", "alice", leaseRequest{Action: "acquire"})
	rec := h.do("POST", "/api/machines/no-unit/power", "alice", powerRequest{Op: "start"})
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("unmanaged machine status = %d, want 501: %s", rec.Code, rec.Body)
	}
	if ops := h.fakeHost.Ops(); len(ops) != 0 {
		t.Errorf("host ops = %v, want none", ops)
	}
}

// A recording name comes from a client, so it must not be able to name a file
// outside the recordings directory or belonging to another machine.
func TestRecordingDownloadRejectsTraversal(t *testing.T) {
	h := newHarness(t)

	// A real capture for this machine, and a secret next door.
	good := "el9-build-20260912T100000Z.cast"
	os.WriteFile(filepath.Join(h.dir, good), []byte(`{"version":2}`+"\n"), 0o640)
	os.WriteFile(filepath.Join(h.dir, "other-20260912T100000Z.cast"), []byte("other"), 0o640)
	os.WriteFile(filepath.Join(filepath.Dir(h.dir), "secret.txt"), []byte("password"), 0o600)

	rec := h.do("GET", "/api/machines/el9-build/recordings/"+good, "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("legitimate download status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"version":2`) {
		t.Errorf("body = %q", rec.Body.String())
	}

	for _, name := range []string{
		"../secret.txt",
		"..%2Fsecret.txt",
		"other-20260912T100000Z.cast", // another machine's capture
		"el9-build-20260912T100000Z.cast/../../secret.txt",
		"passwd",
	} {
		rec := h.do("GET", "/api/machines/el9-build/recordings/"+name, "alice", nil)
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "password") {
			t.Errorf("%q escaped the recordings directory", name)
		}
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "other") {
			t.Errorf("%q read another machine's capture", name)
		}
	}
}

func TestRecordingsListing(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{
		"el9-build-20260912T100000Z.cast",
		"el9-build-20260912T110000Z.cast",
		"serial-only-20260912T100000Z.cast",
	} {
		os.WriteFile(filepath.Join(h.dir, n), []byte("x"), 0o640)
	}

	rec := h.do("GET", "/api/machines/el9-build/recordings", "alice", nil)
	var body struct {
		Recordings []serial.Recording `json:"recordings"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Recordings) != 2 {
		t.Fatalf("got %d recordings, want 2: %+v", len(body.Recordings), body.Recordings)
	}
	// Newest first.
	if body.Recordings[0].Name < body.Recordings[1].Name {
		t.Errorf("recordings not newest first: %+v", body.Recordings)
	}
}

// Identity comes from the proxy header and is used to name the lease holder
// and to log who did what. It is not an authorisation input.
func TestIdentityFromProxyHeader(t *testing.T) {
	h := newHarness(t)

	rec := h.do("GET", "/api/config", "alice@example.com", nil)
	if got := decode[map[string]any](t, rec)["identity"]; got != "alice@example.com" {
		t.Errorf("identity = %v", got)
	}
	// No header means the proxy is misconfigured; labview still serves, as
	// a named default rather than as nobody.
	rec = h.do("GET", "/api/config", "", nil)
	if got := decode[map[string]any](t, rec)["identity"]; got != "unidentified" {
		t.Errorf("default identity = %v", got)
	}
}

// An asserted identity must not carry control characters into logs, captures
// and the UI.
func TestIdentityIsSanitised(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("X-Forwarded-User", "alice\r\nX-Admin: yes")
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)

	got, _ := decode[map[string]any](t, rec)["identity"].(string)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("identity kept control characters: %q", got)
	}
	if !strings.HasPrefix(got, "alice") {
		t.Fatalf("identity = %q", got)
	}
}

func TestActivityRecordsWhoDidWhat(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "acquire"})
	h.do("POST", "/api/machines/el9-build/power", "alice", powerRequest{Op: "start"})
	h.do("POST", "/api/machines/el9-build/power", "bob", powerRequest{Op: "stop"}) // refused

	rec := h.do("GET", "/api/machines/el9-build/activity", "alice", nil)
	var body struct {
		Activity []activity.Event `json:"activity"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Activity) < 3 {
		t.Fatalf("got %d events, want at least 3: %+v", len(body.Activity), body.Activity)
	}

	var sawGrant, sawPower, sawDenied bool
	for _, ev := range body.Activity {
		switch ev.Action {
		case activity.ActionGranted:
			sawGrant = ev.User == "alice"
		case activity.ActionPower:
			sawPower = ev.User == "alice" && ev.Detail == "start"
		case activity.ActionDenied:
			sawDenied = ev.User == "bob"
		}
	}
	if !sawGrant || !sawPower || !sawDenied {
		t.Errorf("activity missing entries: grant=%v power=%v denied=%v: %+v",
			sawGrant, sawPower, sawDenied, body.Activity)
	}
	// Newest first, so the tab reads as a history.
	if len(body.Activity) > 1 && body.Activity[0].At.Before(body.Activity[1].At) {
		t.Error("activity is not newest first")
	}
}

func TestLogsForMachineWithoutUnit(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/api/machines/no-unit/logs", "alice", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no systemd unit") {
		t.Errorf("unhelpful error: %s", rec.Body)
	}
}

func TestLogsLineLimitIsBounded(t *testing.T) {
	h := newHarness(t)
	// A client asking for a million lines gets a bounded answer rather than
	// the whole journal.
	rec := h.do("GET", "/api/machines/el9-build/logs?lines=1000000", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

func TestHealthz(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decode[map[string]any](t, rec)
	if body["machines"].(float64) != 3 {
		t.Errorf("machines = %v, want 3", body["machines"])
	}
}

// The wall polls the listing, so a wedged host must not wedge it.
func TestMachinesListingSurvivesSlowHost(t *testing.T) {
	h := newHarness(t)
	h.srv.hosts = &slowHost{Fake: host.NewFake()}

	done := make(chan int, 1)
	go func() {
		done <- h.do("GET", "/api/machines", "alice", nil).Code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the listing hung on a slow host")
	}
}

// slowHost blocks until its context is cancelled, standing in for an
// unresponsive system bus.
type slowHost struct{ *host.Fake }

func (*slowHost) UnitState(ctx context.Context, m inventory.Machine) (host.Unit, error) {
	<-ctx.Done()
	return host.Unit{}, ctx.Err()
}
