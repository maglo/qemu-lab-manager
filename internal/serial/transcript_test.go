package serial

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readEvents(t *testing.T, path string) (asciicastHeader, []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	if !sc.Scan() {
		t.Fatal("no header line")
	}
	var hdr asciicastHeader
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
		t.Fatalf("header is not JSON: %v (%q)", err, sc.Text())
	}

	var out []string
	for sc.Scan() {
		// Every event must be a well formed [time, "o", data] triple, or a
		// player will reject the file.
		var ev []json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("event is not JSON: %v (%q)", err, sc.Text())
		}
		if len(ev) != 3 {
			t.Fatalf("event has %d fields, want 3: %q", len(ev), sc.Text())
		}
		var kind, data string
		var elapsed float64
		if err := json.Unmarshal(ev[0], &elapsed); err != nil {
			t.Fatalf("event time: %v", err)
		}
		if err := json.Unmarshal(ev[1], &kind); err != nil || kind != "o" {
			t.Fatalf("event kind = %q, want \"o\"", kind)
		}
		if err := json.Unmarshal(ev[2], &data); err != nil {
			t.Fatalf("event data: %v", err)
		}
		out = append(out, data)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return hdr, out
}

func TestTranscriptWritesValidAsciicast(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

	tr, err := NewTranscript("el9-build", "el9-build serial", TranscriptPolicy{Dir: dir}, start)
	if err != nil {
		t.Fatalf("NewTranscript: %v", err)
	}
	tr.Write([]byte("[    0.000000] Linux version 6.1\r\n"), start.Add(200*time.Millisecond))
	tr.Write([]byte("login: "), start.Add(2*time.Second))
	if err := tr.Close(start.Add(3 * time.Second)); err != nil {
		t.Fatalf("Close: %v", err)
	}

	hdr, events := readEvents(t, tr.Path())
	if hdr.Version != 2 {
		t.Fatalf("version = %d, want 2", hdr.Version)
	}
	if hdr.Timestamp != start.Unix() {
		t.Fatalf("timestamp = %d, want %d", hdr.Timestamp, start.Unix())
	}
	want := []string{"[    0.000000] Linux version 6.1\r\n", "login: "}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %q", len(events), len(want), events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event %d = %q, want %q", i, events[i], want[i])
		}
	}

	// The filename must carry machine id and start time, since retention
	// and the recordings tab both read it.
	base := filepath.Base(tr.Path())
	if base != "el9-build-20260912T100000Z.cast" {
		t.Fatalf("filename = %q", base)
	}
}

// A rune split across two reads must survive intact. This is the case the
// held-back prefix exists for, so test every possible split point rather than
// one hand-picked offset -- an offset that happens to fall on a rune boundary
// passes trivially and proves nothing.
func TestTranscriptRejoinsRuneSplitAcrossWrites(t *testing.T) {
	// Two byte, three byte and four byte runes, so every continuation
	// length is exercised.
	const text = "boot: \u00e4\u00b0 \u2502 \U0001f600 done\r\n"
	full := []byte(text)

	for split := 1; split < len(full); split++ {
		t.Run(fmt.Sprintf("split@%d", split), func(t *testing.T) {
			dir := t.TempDir()
			start := time.Now()
			tr, err := NewTranscript("m", "", TranscriptPolicy{Dir: dir}, start)
			if err != nil {
				t.Fatalf("NewTranscript: %v", err)
			}
			tr.Write(full[:split], start)
			tr.Write(full[split:], start.Add(time.Millisecond))
			tr.Close(start.Add(time.Second))

			_, events := readEvents(t, tr.Path())
			joined := strings.Join(events, "")
			if joined != text {
				t.Fatalf("joined = %q, want %q", joined, text)
			}
			if strings.Contains(joined, "\ufffd") {
				t.Fatalf("valid text corrupted with U+FFFD at split %d: %q", split, joined)
			}
		})
	}
}

// Genuinely invalid bytes cannot go into a JSON string, so they become
// U+FFFD -- but they must not be held back waiting for a continuation, and
// must not corrupt the surrounding text.
func TestTranscriptReplacesInvalidBytes(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	tr, _ := NewTranscript("m", "", TranscriptPolicy{Dir: dir}, start)

	// 0xFF is never a valid UTF-8 byte anywhere.
	tr.Write([]byte{'o', 'k', 0xFF, 'a', 'f', 't', 'e', 'r'}, start)
	tr.Close(start.Add(time.Second))

	_, events := readEvents(t, tr.Path())
	got := strings.Join(events, "")
	if !strings.HasPrefix(got, "ok") || !strings.HasSuffix(got, "after") {
		t.Fatalf("surrounding text lost: %q", got)
	}
	if !strings.Contains(got, "�") {
		t.Fatalf("invalid byte was dropped rather than replaced: %q", got)
	}
}

func TestTranscriptFlushesHeldBytesOnClose(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	tr, _ := NewTranscript("m", "", TranscriptPolicy{Dir: dir}, start)

	// A lone lead byte: a continuation is expected but never arrives.
	tr.Write([]byte{'x', 0xC3}, start)
	tr.Close(start.Add(time.Second))

	_, events := readEvents(t, tr.Path())
	got := strings.Join(events, "")
	if !strings.HasPrefix(got, "x") {
		t.Fatalf("text lost: %q", got)
	}
	if !strings.Contains(got, "�") {
		t.Fatalf("held byte was never flushed: %q", got)
	}
}

func TestTranscriptFileCeilingTruncates(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	tr, _ := NewTranscript("m", "", TranscriptPolicy{Dir: dir, MaxFileBytes: 512}, start)

	spew := strings.Repeat("A", 200)
	for i := 0; i < 50; i++ {
		tr.Write([]byte(spew), start.Add(time.Duration(i)*time.Millisecond))
	}
	tr.Close(start.Add(time.Second))

	fi, err := os.Stat(tr.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Bounded, not exact: the ceiling stops further appends rather than
	// splitting the run across files.
	if fi.Size() > 2048 {
		t.Fatalf("file grew to %d bytes despite 512 byte ceiling", fi.Size())
	}
	_, events := readEvents(t, tr.Path())
	if !strings.Contains(strings.Join(events, ""), "truncated") {
		t.Fatal("truncation was not recorded in the capture")
	}
}

func TestTranscriptDisabledWhenNoDir(t *testing.T) {
	tr, err := NewTranscript("m", "", TranscriptPolicy{}, time.Now())
	if err != nil {
		t.Fatalf("NewTranscript: %v", err)
	}
	if tr != nil {
		t.Fatal("expected nil transcript when capture is disabled")
	}
	// A nil transcript must be safe to drive, so the broker needs no
	// branches around it.
	if err := tr.Write([]byte("x"), time.Now()); err != nil {
		t.Fatalf("Write on nil: %v", err)
	}
	if err := tr.Close(time.Now()); err != nil {
		t.Fatalf("Close on nil: %v", err)
	}
	if tr.Path() != "" {
		t.Fatal("Path on nil should be empty")
	}
}

func TestPruneRetainsByCount(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	var names []string
	for i := 0; i < 6; i++ {
		ts := now.Add(time.Duration(-i) * time.Hour).UTC().Format(transcriptTimeFormat)
		name := "m-" + ts + ".cast"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	// An unrelated machine's captures must not be touched.
	other := "other-" + now.UTC().Format(transcriptTimeFormat) + ".cast"
	os.WriteFile(filepath.Join(dir, other), []byte("x"), 0o640)

	if err := Prune(TranscriptPolicy{Dir: dir, MaxFiles: 3}, "m", "", now); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	recs, _ := ListRecordings(dir, "m")
	if len(recs) != 3 {
		t.Fatalf("kept %d captures, want 3", len(recs))
	}
	// Newest three survive.
	for i, r := range recs {
		if r.Name != names[i] {
			t.Fatalf("kept[%d] = %q, want %q", i, r.Name, names[i])
		}
	}
	if _, err := os.Stat(filepath.Join(dir, other)); err != nil {
		t.Fatalf("pruning touched another machine's capture: %v", err)
	}
}

func TestPruneNeverDeletesActiveCapture(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	tr, _ := NewTranscript("m", "", TranscriptPolicy{Dir: dir}, now)

	// Policy that would otherwise delete everything.
	if err := Prune(TranscriptPolicy{Dir: dir, MaxFiles: 1, MaxAge: time.Nanosecond}, "m", tr.Path(), now.Add(time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(tr.Path()); err != nil {
		t.Fatalf("active capture was pruned: %v", err)
	}
}

func TestPruneRetainsByTotalBytes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < 5; i++ {
		ts := now.Add(time.Duration(-i) * time.Hour).UTC().Format(transcriptTimeFormat)
		os.WriteFile(filepath.Join(dir, "m-"+ts+".cast"), make([]byte, 100), 0o640)
	}
	if err := Prune(TranscriptPolicy{Dir: dir, MaxTotalBytes: 250}, "m", "", now); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	recs, _ := ListRecordings(dir, "m")
	if len(recs) != 2 {
		t.Fatalf("kept %d captures, want 2 (250 byte ceiling, 100 bytes each)", len(recs))
	}
}

func TestListRecordingsParsesStartTime(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "m-20260912T100000Z.cast"), []byte("x"), 0o640)
	recs, err := ListRecordings(dir, "m")
	if err != nil {
		t.Fatalf("ListRecordings: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recordings", len(recs))
	}
	want := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	if !recs[0].Started.Equal(want) {
		t.Fatalf("Started = %v, want %v", recs[0].Started, want)
	}
}
