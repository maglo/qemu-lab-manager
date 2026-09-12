package inventory

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultRescan is how often the watcher reads the inventory directory again
// without being told to.
const DefaultRescan = 2 * time.Second

// settleDelay coalesces a burst of file events into one scan. A playbook that
// writes ten machines writes ten files, and the wall wants one reload, not
// ten. It is short enough that a person who saves a file sees the change
// before they reach the browser.
const settleDelay = 50 * time.Millisecond

// Watcher keeps a Set loaded from the inventory directory.
//
// The directory is watched, so a new file and a deleted file reach the wall
// at once. A periodic scan runs as well, because a watch can be lost: the
// directory is replaced, or it lives on a filesystem that reports nothing.
//
// A file that does not load is logged and keeps its previous entry live. One
// machine's typo takes down one machine, and a half-written file takes down
// nothing.
type Watcher struct {
	dir    string
	rescan time.Duration
	log    *slog.Logger

	current atomic.Pointer[Set]

	// loader, fsw and watching belong to the goroutine running Run, and to
	// NewWatcher before that goroutine starts.
	loader   *loader
	fsw      *fsnotify.Watcher
	watching bool

	mu     sync.Mutex
	failed []string
	subs   []chan struct{}
}

// NewWatcher reads the directory once and returns a watcher holding it. The
// first read must succeed -- starting with no machines because the operator
// fat-fingered a file would look like a working but empty lab.
//
// The watch starts before that first read, so a file written while labview
// starts is not missed.
func NewWatcher(dir string, rescan time.Duration, log *slog.Logger) (*Watcher, error) {
	if rescan <= 0 {
		rescan = DefaultRescan
	}
	if log == nil {
		log = slog.Default()
	}
	w := &Watcher{dir: dir, rescan: rescan, log: log, loader: newLoader(dir)}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.log.Warn("cannot watch the inventory directory, rescanning only",
			"dir", dir, "interval", rescan, "error", err)
	} else {
		w.fsw = fsw
		w.watching = w.watch()
	}

	set, _, errs := w.loader.scan()
	if len(errs) > 0 {
		w.Close()
		return nil, errors.Join(errs...)
	}
	w.current.Store(set)
	return w, nil
}

// Close stops the watch. Run closes the watch when its context ends, so only
// a caller that never runs the watcher needs this.
func (w *Watcher) Close() error {
	if w.fsw == nil {
		return nil
	}
	return w.fsw.Close()
}

// Current returns the live set. Callers hold the returned snapshot for as
// long as they like; a reload replaces the pointer rather than mutating it.
func (w *Watcher) Current() *Set { return w.current.Load() }

// Failed names the files of the last scan that did not load. Only the names:
// a message can quote a value from the file, and section 6 keeps the
// addresses off every client.
func (w *Watcher) Failed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.failed))
	copy(out, w.failed)
	return out
}

// Subscribe returns a channel notified after each reload that changes
// something. The channel is buffered and coalescing: a subscriber that is
// busy sees one wakeup, not a queue of them, which is what broker
// reconciliation wants.
func (w *Watcher) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	return ch
}

// Run watches the directory until the context is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	defer w.Close()

	var (
		events   chan fsnotify.Event
		failures chan error
	)
	if w.fsw != nil {
		events, failures = w.fsw.Events, w.fsw.Errors
	}

	ticker := time.NewTicker(w.rescan)
	defer ticker.Stop()

	var settle <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			// A permission change leaves the content alone.
			if event.Op == fsnotify.Chmod {
				continue
			}
			if event.Name == w.dir && event.Has(fsnotify.Remove|fsnotify.Rename) {
				w.watching = false
			}
			settle = time.After(settleDelay)

		case err, ok := <-failures:
			if !ok {
				failures = nil
				continue
			}
			w.log.Warn("the inventory watch reported an error", "dir", w.dir, "error", err)

		case <-settle:
			settle = nil
			w.reload()

		case <-ticker.C:
			if w.fsw != nil && !w.watching {
				w.watching = w.watch()
			}
			w.reload()
		}
	}
}

func (w *Watcher) watch() bool {
	if err := w.fsw.Add(w.dir); err != nil {
		w.log.Warn("cannot watch the inventory directory, rescanning only",
			"dir", w.dir, "interval", w.rescan, "error", err)
		return false
	}
	return true
}

func (w *Watcher) reload() {
	set, changed, errs := w.loader.scan()
	if set == nil {
		// The directory itself is gone or unreadable. Keep the live set:
		// the machines are still running, whatever the filesystem says.
		w.log.Warn("cannot read the inventory directory, keeping the live machines",
			"dir", w.dir, "error", errors.Join(errs...))
		return
	}
	if !changed {
		return
	}

	failed := make([]string, 0, len(errs))
	for _, err := range errs {
		w.log.Error("a machine file did not load", "dir", w.dir, "error", err)
		var fail *FileError
		if errors.As(err, &fail) {
			failed = append(failed, fail.Name)
		}
	}

	w.current.Store(set)

	w.mu.Lock()
	w.failed = failed
	subs := make([]chan struct{}, len(w.subs))
	copy(subs, w.subs)
	w.mu.Unlock()

	w.log.Info("inventory reloaded", "dir", w.dir, "machines", set.Len(), "failed", len(failed))
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // already pending; coalesce
		}
	}
}
