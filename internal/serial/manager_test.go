package serial

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

func mustSet(t *testing.T, doc string) *inventory.Set {
	t.Helper()
	set, err := inventory.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("inventory.Parse: %v", err)
	}
	return set
}

func testManagerConfig() ManagerConfig {
	return ManagerConfig{
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
		Log:        newDiscardLogger(),
	}
}

func TestManagerStartsBrokerPerSerialMachine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, testManagerConfig())
	defer m.Close()

	m.Reconcile(mustSet(t, `[
	  {"id":"a","serial":"/run/a.sock"},
	  {"id":"b","serial":"10.0.0.1:4001"},
	  {"id":"no-serial","vnc":"10.0.0.1:5901"}
	]`))

	if _, ok := m.Get("a"); !ok {
		t.Error("no broker for machine a")
	}
	if _, ok := m.Get("b"); !ok {
		t.Error("no broker for machine b")
	}
	// A machine with no serial address gets no broker: there is nothing to
	// hold open.
	if _, ok := m.Get("no-serial"); ok {
		t.Error("broker created for a machine with no serial address")
	}

	// The dial target must follow from the address shape.
	ba, _ := m.Get("a")
	if net, addr := ba.Address(); net != "unix" || addr != "/run/a.sock" {
		t.Errorf("machine a dials %s/%s, want unix//run/a.sock", net, addr)
	}
	bb, _ := m.Get("b")
	if net, addr := bb.Address(); net != "tcp" || addr != "10.0.0.1:4001" {
		t.Errorf("machine b dials %s/%s, want tcp/10.0.0.1:4001", net, addr)
	}
}

func TestManagerStopsBrokerWhenMachineLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, testManagerConfig())
	defer m.Close()

	m.Reconcile(mustSet(t, `[{"id":"a","serial":"/run/a.sock"}]`))
	b, ok := m.Get("a")
	if !ok {
		t.Fatal("no broker for a")
	}

	m.Reconcile(mustSet(t, `[]`))
	if _, ok := m.Get("a"); ok {
		t.Fatal("broker survived removal from the inventory")
	}
	// The broker must actually be shut down, not merely forgotten -- it
	// holds a socket and a capture file.
	waitFor(t, 2*time.Second, func() bool {
		_, _, err := b.Attach()
		return err != nil
	})
	if _, _, err := b.Attach(); err == nil {
		t.Fatal("removed broker is still running")
	}
}

func TestManagerReplacesBrokerWhenAddressChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, testManagerConfig())
	defer m.Close()

	m.Reconcile(mustSet(t, `[{"id":"a","serial":"/run/old.sock"}]`))
	before, _ := m.Get("a")

	m.Reconcile(mustSet(t, `[{"id":"a","serial":"/run/new.sock"}]`))
	after, ok := m.Get("a")
	if !ok {
		t.Fatal("broker vanished on address change")
	}
	if after == before {
		t.Fatal("broker was reused for a different endpoint")
	}
	if _, addr := after.Address(); addr != "/run/new.sock" {
		t.Fatalf("new broker dials %q", addr)
	}
}

// An unchanged machine must keep its broker: restarting one would drop the
// scrollback and rotate the capture for no reason, every time the playbook
// rewrites the inventory.
func TestManagerKeepsBrokerAcrossUnrelatedReload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, testManagerConfig())
	defer m.Close()

	doc := `[{"id":"a","serial":"/run/a.sock"}]`
	m.Reconcile(mustSet(t, doc))
	before, _ := m.Get("a")

	// Same machine, plus a new one.
	m.Reconcile(mustSet(t, `[
	  {"id":"a","serial":"/run/a.sock"},
	  {"id":"b","serial":"/run/b.sock"}
	]`))

	after, _ := m.Get("a")
	if after != before {
		t.Fatal("unchanged machine's broker was restarted")
	}
	if _, ok := m.Get("b"); !ok {
		t.Fatal("new machine got no broker")
	}
}

func TestManagerStatesReportLiveness(t *testing.T) {
	q := newFakeQEMU(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, testManagerConfig())
	defer m.Close()

	m.Reconcile(mustSet(t, fmt.Sprintf(`[
	  {"id":"live","serial":%q},
	  {"id":"dead","serial":"/run/nonexistent-labview-test.sock"}
	]`, q.path)))

	waitFor(t, 3*time.Second, func() bool { return m.States()["live"].Connected })

	states := m.States()
	if !states["live"].Connected {
		t.Errorf("live machine reports disconnected: %+v", states["live"])
	}
	if states["dead"].Connected {
		t.Errorf("absent machine reports connected: %+v", states["dead"])
	}
	// A machine that is simply switched off should be retrying, not stuck.
	waitFor(t, 2*time.Second, func() bool { return m.States()["dead"].Attempts > 1 })
	if got := m.States()["dead"].Attempts; got < 2 {
		t.Errorf("absent machine attempts = %d, want retries", got)
	}
}

func TestManagerRunFollowsInventoryReload(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/inventory.json"
	write := func(doc string) {
		t.Helper()
		if err := writeFile(path, doc); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"id":"a","serial":"/run/a.sock"}]`)

	w, err := inventory.NewWatcher(path, 20*time.Millisecond, newDiscardLogger())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	m := NewManager(ctx, testManagerConfig())
	go m.Run(ctx, w)

	waitFor(t, 2*time.Second, func() bool { _, ok := m.Get("a"); return ok })
	if _, ok := m.Get("a"); !ok {
		t.Fatal("broker for a never started")
	}

	// The playbook rewrites the file, adding a machine.
	time.Sleep(30 * time.Millisecond) // ensure a distinct mtime
	write(`[{"id":"a","serial":"/run/a.sock"},{"id":"c","serial":"/run/c.sock"}]`)

	waitFor(t, 3*time.Second, func() bool { _, ok := m.Get("c"); return ok })
	if _, ok := m.Get("c"); !ok {
		t.Fatal("broker for c never started after reload")
	}
}
