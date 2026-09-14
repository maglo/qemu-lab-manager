package qmp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultTimeout bounds one exchange end to end. QEMU answers send-key and
// screendump synchronously, so a machine that does not answer quickly is a
// machine that is not answering.
const DefaultTimeout = 5 * time.Second

// ErrNoCaptureDir means labview has nowhere for QEMU to write a screenshot.
var ErrNoCaptureDir = errors.New("no screenshot directory is configured")

// Target is a machine's control socket. The API maps an inventory entry to
// one, so this package needs no inventory of its own.
type Target struct {
	ID      string
	Network string
	Address string
}

// DialFunc opens a control socket. Tests replace it.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Options configures a Manager.
type Options struct {
	// CaptureDir is where QEMU writes a screenshot and labview reads it
	// again. Empty disables capture.
	CaptureDir string
	Timeout    time.Duration
	Dial       DialFunc
}

// Manager runs commands on control sockets.
//
// It holds one lock per machine, because a QMP socket takes one client at a
// time: two tiles asking for a screenshot at once would otherwise leave the
// second one refused by QEMU.
type Manager struct {
	captureDir string
	timeout    time.Duration
	dial       DialFunc

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewManager returns a manager.
func NewManager(opts Options) *Manager {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Dial == nil {
		opts.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		}
	}
	return &Manager{
		captureDir: opts.CaptureDir,
		timeout:    opts.Timeout,
		dial:       opts.Dial,
		locks:      make(map[string]*sync.Mutex),
	}
}

// CaptureEnabled reports whether Screenshot can work at all.
func (m *Manager) CaptureEnabled() bool { return m.captureDir != "" }

// SendKeys presses one chord on a machine.
func (m *Manager) SendKeys(ctx context.Context, t Target, keys []string, holdMs int) error {
	if err := ValidateKeys(keys); err != nil {
		return err
	}
	return m.with(ctx, t, func(ctx context.Context, c *Client) error {
		return c.SendKey(ctx, keys, holdMs)
	})
}

// Screenshot captures the current screen of a machine as PNG.
func (m *Manager) Screenshot(ctx context.Context, t Target) ([]byte, error) {
	if !m.CaptureEnabled() {
		return nil, ErrNoCaptureDir
	}
	dir, err := filepath.Abs(m.captureDir)
	if err != nil {
		return nil, err
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, t.ID+"-"+hex.EncodeToString(buf[:])+".png")

	// QEMU owns the file, so labview removes it as soon as it has read it.
	// A capture left behind is a frame of somebody's screen on disk.
	defer os.Remove(path)

	var png []byte
	err = m.with(ctx, t, func(ctx context.Context, c *Client) error {
		if err := c.Screendump(ctx, path); err != nil {
			return err
		}
		png, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading the capture QEMU wrote: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return png, nil
}

// with runs one exchange on a machine's control socket, one caller at a time.
func (m *Manager) with(ctx context.Context, t Target, fn func(context.Context, *Client) error) error {
	if t.Address == "" {
		return errors.New("the machine has no control socket")
	}

	lock := m.lockFor(t.ID)
	lock.Lock()
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	conn, err := m.dial(ctx, t.Network, t.Address)
	if err != nil {
		return fmt.Errorf("dialling the control socket: %w", err)
	}
	client, err := New(ctx, conn)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()

	return fn(ctx, client)
}

func (m *Manager) lockFor(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, ok := m.locks[id]
	if !ok {
		lock = &sync.Mutex{}
		m.locks[id] = lock
	}
	return lock
}
