package inventory

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPollInterval is how often the watcher stats the inventory file.
// Polling rather than inotify because the design asks only for "re-reads it
// when the mtime changes", and a producer that writes by rename (as a playbook
// does) defeats a watch on the inode anyway.
const DefaultPollInterval = 2 * time.Second

// Watcher keeps a Set loaded from disk, reloading on mtime change.
//
// A failed reload is logged and discarded: the last good set stays live. A
// half-written inventory should not take the console wall down.
type Watcher struct {
	path     string
	interval time.Duration
	log      *slog.Logger

	current atomic.Pointer[Set]

	mu       sync.Mutex
	lastMod  time.Time
	lastSize int64
	subs     []chan struct{}
}

// NewWatcher loads the inventory once and returns a watcher holding it. The
// initial load must succeed -- starting with no machines because the operator
// fat-fingered the file would look like a working but empty lab.
func NewWatcher(path string, interval time.Duration, log *slog.Logger) (*Watcher, error) {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	if log == nil {
		log = slog.Default()
	}
	w := &Watcher{path: path, interval: interval, log: log}

	set, err := Load(path)
	if err != nil {
		return nil, err
	}
	w.current.Store(set)
	if fi, err := os.Stat(path); err == nil {
		w.lastMod, w.lastSize = fi.ModTime(), fi.Size()
	}
	return w, nil
}

// Current returns the live set. Callers hold the returned snapshot for as
// long as they like; a reload replaces the pointer rather than mutating it.
func (w *Watcher) Current() *Set { return w.current.Load() }

// Subscribe returns a channel notified after each successful reload. The
// channel is buffered and coalescing: a subscriber that is busy sees one
// wakeup, not a queue of them, which is what broker reconciliation wants.
func (w *Watcher) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	return ch
}

// Run polls until the context is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.reloadIfChanged()
		}
	}
}

func (w *Watcher) reloadIfChanged() {
	fi, err := os.Stat(w.path)
	if err != nil {
		w.log.Warn("inventory stat failed", "path", w.path, "error", err)
		return
	}

	w.mu.Lock()
	unchanged := fi.ModTime().Equal(w.lastMod) && fi.Size() == w.lastSize
	w.mu.Unlock()
	if unchanged {
		return
	}

	set, err := Load(w.path)
	if err != nil {
		// Note the mtime anyway. Otherwise a file that stays broken is
		// re-read and re-logged on every tick.
		w.mu.Lock()
		w.lastMod, w.lastSize = fi.ModTime(), fi.Size()
		w.mu.Unlock()
		w.log.Error("inventory reload failed, keeping previous set",
			"path", w.path, "error", err)
		return
	}

	w.current.Store(set)

	w.mu.Lock()
	w.lastMod, w.lastSize = fi.ModTime(), fi.Size()
	subs := make([]chan struct{}, len(w.subs))
	copy(subs, w.subs)
	w.mu.Unlock()

	w.log.Info("inventory reloaded", "path", w.path, "machines", set.Len())
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // already pending; coalesce
		}
	}
}
