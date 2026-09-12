package serial

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// TranscriptPolicy configures capture and retention.
//
// Retention is by count and age, with a size ceiling as a backstop against a
// machine that spews (design section 11).
type TranscriptPolicy struct {
	Dir string // empty disables capture

	MaxFiles      int           // per machine; 0 = unlimited
	MaxAge        time.Duration // 0 = unlimited
	MaxTotalBytes int64         // per machine, across files; 0 = unlimited

	// MaxFileBytes bounds a single capture. The design forbids rotating on
	// size -- one file is one VM run -- so exceeding this stops appending
	// and marks the file truncated rather than starting a second file for
	// the same run.
	MaxFileBytes int64

	// FlushInterval bounds how long output may sit in the writer's buffer
	// before reaching the disk. A capture has to be readable while the
	// machine is still running, because reading a run that went wrong
	// usually happens while it is still going.
	FlushInterval time.Duration
}

// DefaultFlushInterval is how often an open capture is pushed to disk.
const DefaultFlushInterval = 2 * time.Second

// Flush interval, with the default applied.
func (p TranscriptPolicy) flushInterval() time.Duration {
	if p.FlushInterval <= 0 {
		return DefaultFlushInterval
	}
	return p.FlushInterval
}

// Enabled reports whether captures should be written at all.
func (p TranscriptPolicy) Enabled() bool { return p.Dir != "" }

// asciicastHeader is the first line of an asciicast v2 file.
type asciicastHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Transcript writes one asciicast v2 file, covering one upstream connection.
//
// The broker owns the fd and rotates in process, so nothing external should
// touch these files -- copytruncate against a live writer loses data (design
// section 11).
type Transcript struct {
	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	path    string
	started time.Time

	written   int64
	truncated bool
	closed    bool

	// pending holds a trailing byte sequence that is a valid prefix of a
	// multi-byte rune but not yet complete.
	//
	// asciicast stores output as JSON strings, which must be UTF-8, while a
	// serial line is a byte stream that can split a rune across reads.
	// Holding the prefix back until the next chunk keeps a rune that arrived
	// in two pieces intact in the capture. The websocket path does no such
	// thing: it forwards raw bytes and lets the browser decode incrementally
	// (design section 3).
	pending []byte

	policy TranscriptPolicy
}

// transcriptTimeFormat is filesystem-safe and sorts lexicographically in time
// order, which is what makes retention pruning a sort on names.
const transcriptTimeFormat = "20060102T150405Z"

// NewTranscript creates a capture file for a machine run starting now.
func NewTranscript(machineID, title string, policy TranscriptPolicy, now time.Time) (*Transcript, error) {
	if !policy.Enabled() {
		return nil, nil
	}
	if err := os.MkdirAll(policy.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("transcript dir: %w", err)
	}

	name := fmt.Sprintf("%s-%s.cast", machineID, now.UTC().Format(transcriptTimeFormat))
	path := filepath.Join(policy.Dir, name)

	// O_EXCL so that two runs starting inside the same second cannot
	// interleave into one file; fall back to a suffixed name.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		for i := 1; i < 10 && os.IsExist(err); i++ {
			alt := fmt.Sprintf("%s-%s.%d.cast", machineID, now.UTC().Format(transcriptTimeFormat), i)
			path = filepath.Join(policy.Dir, alt)
			f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		}
		if err != nil {
			return nil, fmt.Errorf("transcript create: %w", err)
		}
	}

	t := &Transcript{
		f:       f,
		w:       bufio.NewWriterSize(f, 32<<10),
		path:    path,
		started: now,
		policy:  policy,
	}

	hdr := asciicastHeader{
		Version:   2,
		Width:     80,
		Height:    24,
		Timestamp: now.Unix(),
		Title:     title,
		Env:       map[string]string{"TERM": "vt100"},
	}
	line, err := json.Marshal(hdr)
	if err != nil {
		f.Close()
		return nil, err
	}
	if _, err := t.w.Write(append(line, '\n')); err != nil {
		f.Close()
		return nil, err
	}
	t.written = int64(len(line)) + 1

	// Flush the header at once, so the file is valid asciicast from the
	// moment it exists rather than a zero byte file until the first 32KB of
	// output or the connection dropping.
	if err := t.w.Flush(); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

// Path returns the capture's filename.
func (t *Transcript) Path() string {
	if t == nil {
		return ""
	}
	return t.path
}

// Write records an output event at the given time.
//
// Timestamps are carried in the event structure, never interleaved into the
// byte stream: mixing them in corrupts replay and makes the file useless as a
// serial capture (design section 11).
func (t *Transcript) Write(p []byte, now time.Time) error {
	if t == nil || len(p) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if t.policy.MaxFileBytes > 0 && t.written >= t.policy.MaxFileBytes {
		if !t.truncated {
			t.truncated = true
			t.emit(time.Now(), []byte("\r\n[labview: capture truncated, file size ceiling reached]\r\n"))
			t.w.Flush()
		}
		return nil
	}

	chunk := p
	if len(t.pending) > 0 {
		chunk = append(t.pending, p...)
		t.pending = nil
	}

	// Hold back a trailing incomplete rune so it can be completed by the
	// next chunk instead of becoming a replacement character.
	if held := incompleteSuffix(chunk); held > 0 {
		t.pending = append([]byte(nil), chunk[len(chunk)-held:]...)
		chunk = chunk[:len(chunk)-held]
	}
	if len(chunk) == 0 {
		return nil
	}
	return t.emit(now, chunk)
}

// emit writes one asciicast output event. Caller holds the lock.
func (t *Transcript) emit(now time.Time, chunk []byte) error {
	elapsed := now.Sub(t.started).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}

	// Invalid bytes become U+FFFD. They cannot survive a JSON string, and
	// silently dropping them would misrepresent the line.
	text := strings.ToValidUTF8(string(chunk), "�")
	data, err := json.Marshal(text)
	if err != nil {
		return err
	}

	line := make([]byte, 0, len(data)+32)
	line = append(line, '[')
	line = append(line, []byte(fmt.Sprintf("%.6f", elapsed))...)
	line = append(line, `, "o", `...)
	line = append(line, data...)
	line = append(line, ']', '\n')

	n, err := t.w.Write(line)
	t.written += int64(n)
	return err
}

// incompleteSuffix returns the length of a trailing byte sequence that is a
// valid prefix of a multi-byte rune but not a complete one. It returns 0 when
// the tail is complete, or when it is simply invalid -- garbage is emitted
// immediately rather than waiting for a continuation that will never come.
func incompleteSuffix(b []byte) int {
	// A rune is at most 4 bytes, so only the last 3 can be a partial one.
	for back := 1; back <= 3 && back <= len(b); back++ {
		i := len(b) - back
		c := b[i]
		if utf8.RuneStart(c) {
			if r, size := utf8.DecodeRune(b[i:]); r == utf8.RuneError && size <= 1 {
				// Either a bad lead byte (emit now) or a truncated
				// sequence (hold back). expectedLen tells them apart.
				if want := expectedLen(c); want > len(b)-i {
					return back
				}
				return 0
			}
			return 0
		}
	}
	return 0
}

// expectedLen returns the total length of the rune a lead byte introduces, or
// 0 if the byte is not a valid lead byte.
func expectedLen(c byte) int {
	switch {
	case c < 0x80:
		return 1
	case c >= 0xC2 && c <= 0xDF:
		return 2
	case c >= 0xE0 && c <= 0xEF:
		return 3
	case c >= 0xF0 && c <= 0xF4:
		return 4
	}
	return 0
}

// Flush pushes buffered output to the file.
func (t *Transcript) Flush() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	return t.w.Flush()
}

// Close flushes any held-back bytes and closes the file.
func (t *Transcript) Close(now time.Time) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	// A partial rune at end of capture will never be completed.
	if len(t.pending) > 0 {
		t.emit(now, t.pending)
		t.pending = nil
	}
	t.closed = true
	if err := t.w.Flush(); err != nil {
		t.f.Close()
		return err
	}
	return t.f.Close()
}

// Recording describes a capture on disk, for the recordings tab.
type Recording struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Started time.Time `json:"started"`
	ModTime time.Time `json:"modified"`
}

// recordingName matches what NewTranscript produces: a machine id, a start
// timestamp, an optional collision counter, and the extension.
//
// It is anchored on the timestamp rather than on the id, because an id may
// contain hyphens. A bare prefix match would let machine "el9" claim
// "el9-build-<stamp>.cast", which is a realistic pair of names in one lab.
var recordingName = regexp.MustCompile(`^(.+)-(\d{8}T\d{6}Z)(?:\.\d+)?\.cast$`)

// RecordingBelongsTo reports whether a capture filename is one of machineID's.
//
// The name reaches this from a client, so it is parsed rather than trusted:
// it must be a bare filename of exactly the shape the transcript writer
// produces, for exactly this machine.
func RecordingBelongsTo(name, machineID string) bool {
	if name == "" || len(name) > 256 || machineID == "" {
		return false
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	m := recordingName.FindStringSubmatch(name)
	return m != nil && m[1] == machineID
}

// ListRecordings returns a machine's captures, newest first.
func ListRecordings(dir, machineID string) ([]Recording, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	out := []Recording{}
	for _, e := range entries {
		if e.IsDir() || !RecordingBelongsTo(e.Name(), machineID) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Recording{
			Name:    e.Name(),
			Size:    fi.Size(),
			Started: startTimeFromName(e.Name()),
			ModTime: fi.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

func startTimeFromName(name string) time.Time {
	m := recordingName.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}
	}
	ts, err := time.Parse(transcriptTimeFormat, m[2])
	if err != nil {
		return time.Time{}
	}
	return ts
}

// Prune applies the retention policy to one machine's captures. The active
// capture is passed so it is never deleted from under the broker.
func Prune(policy TranscriptPolicy, machineID, activePath string, now time.Time) error {
	if !policy.Enabled() {
		return nil
	}
	recs, err := ListRecordings(policy.Dir, machineID)
	if err != nil {
		return err
	}

	activeName := filepath.Base(activePath)
	// Newest first; walk forward deciding what to keep.
	var kept int
	var total int64
	var errs []error
	for _, r := range recs {
		if r.Name == activeName {
			kept++
			total += r.Size
			continue
		}

		drop := false
		if policy.MaxFiles > 0 && kept >= policy.MaxFiles {
			drop = true
		}
		if policy.MaxAge > 0 && !r.ModTime.IsZero() && now.Sub(r.ModTime) > policy.MaxAge {
			drop = true
		}
		if policy.MaxTotalBytes > 0 && total+r.Size > policy.MaxTotalBytes {
			drop = true
		}

		if !drop {
			kept++
			total += r.Size
			continue
		}
		if err := os.Remove(filepath.Join(policy.Dir, r.Name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
