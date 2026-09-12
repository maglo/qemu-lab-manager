package serial

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

// ManagerConfig holds the per-broker settings the manager applies to every
// machine it starts.
type ManagerConfig struct {
	RingBytes       int
	SubscriberQueue int
	BackoffMin      time.Duration
	BackoffMax      time.Duration
	DialTimeout     time.Duration
	Transcripts     TranscriptPolicy

	// Dial is injectable for tests.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	Log *slog.Logger
}

// Manager owns one broker per machine that has a serial address.
//
// Brokers start eagerly, at process start rather than on first subscriber,
// because the deciding case is a VM that reboots while nobody is watching
// (design section 11).
type Manager struct {
	cfg ManagerConfig
	log *slog.Logger

	ctx context.Context

	mu      sync.Mutex
	brokers map[string]*runningBroker
	closed  bool
}

type runningBroker struct {
	broker  *Broker
	cancel  context.CancelFunc
	done    chan struct{}
	network string
	address string
}

// NewManager returns a manager bound to ctx. Every broker it starts stops when
// ctx is cancelled.
func NewManager(ctx context.Context, cfg ManagerConfig) *Manager {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Manager{
		cfg:     cfg,
		log:     cfg.Log,
		ctx:     ctx,
		brokers: make(map[string]*runningBroker),
	}
}

// Get returns the broker for a machine, if it has one.
func (m *Manager) Get(id string) (*Broker, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rb, ok := m.brokers[id]
	if !ok {
		return nil, false
	}
	return rb.broker, true
}

// Reconcile brings the running brokers into line with an inventory set:
// starting brokers for new machines, stopping them for departed ones, and
// replacing one whose serial address changed.
//
// It is called once at startup and again after every successful inventory
// reload, which is what makes the inventory file the live source of truth.
func (m *Manager) Reconcile(set *inventory.Set) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}

	wanted := make(map[string]inventory.Machine)
	for _, mach := range set.Machines() {
		if mach.HasSerial() {
			wanted[mach.ID] = mach
		}
	}

	var stop []*runningBroker
	for id, rb := range m.brokers {
		mach, keep := wanted[id]
		if !keep {
			stop = append(stop, rb)
			delete(m.brokers, id)
			m.log.Info("serial broker stopping, machine left the inventory", "machine", id)
			continue
		}
		if rb.network != mach.SerialNetwork() || rb.address != mach.Serial {
			// A different endpoint is a different machine as far as the
			// broker is concerned: its scrollback and its capture describe
			// somewhere else.
			stop = append(stop, rb)
			delete(m.brokers, id)
			m.log.Info("serial broker restarting, address changed", "machine", id,
				"from", rb.address, "to", mach.Serial)
			continue
		}
		delete(wanted, id) // already running, unchanged
	}

	var started []string
	for id, mach := range wanted {
		rb := m.start(mach)
		m.brokers[id] = rb
		started = append(started, id)
	}
	m.mu.Unlock()

	// Stop outside the lock: shutdown closes subscribers, and a subscriber
	// closing calls back into the broker.
	for _, rb := range stop {
		rb.cancel()
	}
	for _, id := range started {
		m.log.Info("serial broker started", "machine", id)
	}
}

// start launches a broker. Caller holds the lock.
func (m *Manager) start(mach inventory.Machine) *runningBroker {
	ctx, cancel := context.WithCancel(m.ctx)
	b := NewBroker(Config{
		MachineID:       mach.ID,
		Title:           mach.DisplayName() + " serial",
		Network:         mach.SerialNetwork(),
		Address:         mach.Serial,
		RingBytes:       m.cfg.RingBytes,
		SubscriberQueue: m.cfg.SubscriberQueue,
		BackoffMin:      m.cfg.BackoffMin,
		BackoffMax:      m.cfg.BackoffMax,
		DialTimeout:     m.cfg.DialTimeout,
		Transcripts:     m.cfg.Transcripts,
		Dial:            m.cfg.Dial,
		Log:             m.log,
	})

	rb := &runningBroker{
		broker:  b,
		cancel:  cancel,
		done:    make(chan struct{}),
		network: mach.SerialNetwork(),
		address: mach.Serial,
	}
	go func() {
		defer close(rb.done)
		b.Run(ctx)
	}()
	return rb
}

// States returns every broker's upstream state, keyed by machine id. The
// machines API reports liveness from this.
func (m *Manager) States() map[string]UpstreamState {
	m.mu.Lock()
	brokers := make(map[string]*Broker, len(m.brokers))
	for id, rb := range m.brokers {
		brokers[id] = rb.broker
	}
	m.mu.Unlock()

	out := make(map[string]UpstreamState, len(brokers))
	for id, b := range brokers {
		out[id] = b.State()
	}
	return out
}

// Run keeps brokers in step with the inventory until ctx is cancelled.
func (m *Manager) Run(ctx context.Context, w *inventory.Watcher) {
	changed := w.Subscribe()
	m.Reconcile(w.Current())
	for {
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-changed:
			m.Reconcile(w.Current())
		}
	}
}

// Close stops every broker and waits for them to finish, so that transcripts
// are flushed and closed before the process exits.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	all := make([]*runningBroker, 0, len(m.brokers))
	for _, rb := range m.brokers {
		all = append(all, rb)
	}
	m.brokers = make(map[string]*runningBroker)
	m.mu.Unlock()

	for _, rb := range all {
		rb.cancel()
	}
	for _, rb := range all {
		select {
		case <-rb.done:
		case <-time.After(2 * time.Second):
			m.log.Warn("serial broker did not stop promptly", "machine", rb.broker.MachineID())
		}
	}
}
