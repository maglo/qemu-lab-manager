package serial

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

// mustSet writes one file per machine and loads the directory, which is the
// path the service itself takes.
func mustSet(t *testing.T, files map[string]string) *inventory.Set {
	t.Helper()
	dir := t.TempDir()
	for id, doc := range files {
		if err := writeFile(filepath.Join(dir, id+".yaml"), doc); err != nil {
			t.Fatal(err)
		}
	}
	set, err := inventory.LoadDir(dir)
	if err != nil {
		t.Fatalf("inventory.LoadDir: %v", err)
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

	m.Reconcile(mustSet(t, map[string]string{
		"a":         "serial: /run/a.sock\n",
		"b":         "serial: 10.0.0.1:4001\n",
		"no-serial": "vnc: 10.0.0.1:5901\n",
	}))

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

	m.Reconcile(mustSet(t, map[string]string{"a": "serial: /run/a.sock\n"}))
	b, ok := m.Get("a")
	if !ok {
		t.Fatal("no broker for a")
	}

	m.Reconcile(mustSet(t, nil))
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

	m.Reconcile(mustSet(t, map[string]string{"a": "serial: /run/old.sock\n"}))
	before, _ := m.Get("a")

	m.Reconcile(mustSet(t, map[string]string{"a": "serial: /run/new.sock\n"}))
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

	m.Reconcile(mustSet(t, map[string]string{"a": "serial: /run/a.sock\n"}))
	before, _ := m.Get("a")

	// Same machine, plus a new one.
	m.Reconcile(mustSet(t, map[string]string{
		"a": "serial: /run/a.sock\n",
		"b": "serial: /run/b.sock\n",
	}))

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

	m.Reconcile(mustSet(t, map[string]string{
		"live": fmt.Sprintf("serial: %s\n", q.path),
		"dead": "serial: /run/nonexistent-labview-test.sock\n",
	}))

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
	write := func(id, doc string) {
		t.Helper()
		if err := writeFile(filepath.Join(dir, id+".yaml"), doc); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "serial: /run/a.sock\n")

	w, err := inventory.NewWatcher(dir, time.Hour, newDiscardLogger())
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

	// The playbook adds a machine, which is one new file.
	write("c", "serial: /run/c.sock\n")

	waitFor(t, 3*time.Second, func() bool { _, ok := m.Get("c"); return ok })
	if _, ok := m.Get("c"); !ok {
		t.Fatal("broker for c never started after reload")
	}
}
