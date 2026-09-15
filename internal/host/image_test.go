package host

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// qcow2Image writes an image with the header fields readImageInfo reads. The
// test builds the bytes itself, so it checks the offsets against the
// specification and not against another reader.
func qcow2Image(t *testing.T, path string, virtual uint64, backing string) {
	t.Helper()
	const headerLen = 512
	img := make([]byte, headerLen)
	copy(img[0:4], qcow2Magic)
	binary.BigEndian.PutUint32(img[4:8], 3)
	binary.BigEndian.PutUint32(img[20:24], 16) // cluster_bits
	binary.BigEndian.PutUint64(img[24:32], virtual)
	if backing != "" {
		binary.BigEndian.PutUint64(img[8:16], headerLen)
		binary.BigEndian.PutUint32(img[16:20], uint32(len(backing)))
		img = append(img, backing...)
	}
	if err := os.WriteFile(path, img, 0o640); err != nil {
		t.Fatal(err)
	}
}

// The virtual size is a header field, so labview reads it without qemu-img.
func TestReadImageInfoReadsTheQCOW2VirtualSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "el9.qcow2")
	const twelveGiB = 12 << 30
	qcow2Image(t, path, twelveGiB, "")

	info, err := readImageInfo(path)
	if err != nil {
		t.Fatalf("readImageInfo: %v", err)
	}
	if info.Format != "qcow2" {
		t.Errorf("format = %q, want qcow2", info.Format)
	}
	if info.VirtualSize != twelveGiB {
		t.Errorf("virtual size = %d, want %d", info.VirtualSize, twelveGiB)
	}
	if info.ActualSize <= 0 {
		t.Errorf("actual size = %d, want the space the file occupies", info.ActualSize)
	}
	if info.BackingFile != "" {
		t.Errorf("backing file = %q, want none", info.BackingFile)
	}
}

// An overlay names its backing file, which is how a reader sees where the
// content lives.
func TestReadImageInfoNamesTheBackingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.qcow2")
	qcow2Image(t, path, 8<<30, "/var/lib/labview/base.qcow2")

	info, err := readImageInfo(path)
	if err != nil {
		t.Fatalf("readImageInfo: %v", err)
	}
	if info.BackingFile != "/var/lib/labview/base.qcow2" {
		t.Errorf("backing file = %q", info.BackingFile)
	}
	if info.VirtualSize != 8<<30 {
		t.Errorf("virtual size = %d, want %d", info.VirtualSize, int64(8<<30))
	}
}

// A file without the magic is raw, and a raw image presents its own length.
// A sparse one occupies less than that, which is the point of both numbers.
func TestReadImageInfoReadsRawAsItsLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const oneGiB = 1 << 30
	if err := f.Truncate(oneGiB); err != nil {
		t.Fatal(err)
	}
	f.Close()

	info, err := readImageInfo(path)
	if err != nil {
		t.Fatalf("readImageInfo: %v", err)
	}
	if info.Format != "raw" {
		t.Errorf("format = %q, want raw", info.Format)
	}
	if info.VirtualSize != oneGiB {
		t.Errorf("virtual size = %d, want %d", info.VirtualSize, int64(oneGiB))
	}
	if info.ActualSize >= oneGiB {
		t.Errorf("a sparse file reports %d on disk, want less than %d", info.ActualSize, int64(oneGiB))
	}
}

// A file shorter than the header is not a qcow2, and reading it is not an
// error.
func TestReadImageInfoOnAShortFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stub.img")
	if err := os.WriteFile(path, []byte("QFI"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := readImageInfo(path)
	if err != nil {
		t.Fatalf("readImageInfo: %v", err)
	}
	if info.Format != "raw" || info.VirtualSize != 3 {
		t.Errorf("got %+v, want raw and 3 bytes", info)
	}
}

func TestReadImageInfoOnAMissingFile(t *testing.T) {
	if _, err := readImageInfo(filepath.Join(t.TempDir(), "gone.qcow2")); err == nil {
		t.Error("reading a nonexistent image succeeded")
	}
}

// One unreadable image costs one row, not the whole details tab.
func TestEnrichDisksRecordsOneErrorForOneImage(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "vm1.qcow2")
	qcow2Image(t, good, 20<<30, "")

	l := NewLocal(LocalOptions{Log: discardLogger()})
	d := Details{Disks: []Disk{
		{Path: good},
		{Path: filepath.Join(dir, "missing.qcow2")},
	}}
	l.enrichDisks(&d)

	if d.Disks[0].VirtualSize != 20<<30 {
		t.Errorf("virtual size = %d, want %d", d.Disks[0].VirtualSize, int64(20<<30))
	}
	if d.Disks[0].Error != "" {
		t.Errorf("a readable image reported %q", d.Disks[0].Error)
	}
	if d.Disks[1].Error == "" {
		t.Error("a missing image reported no error")
	}
}

// The command line says what QEMU was told, and the header says what the
// file holds. The command line wins, because a machine runs the format it
// was given. A reader who sees raw against a qcow2 header sees a real
// misconfiguration.
func TestEnrichDisksKeepsTheFormatFromTheCommandLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm1.img")
	qcow2Image(t, path, 4<<30, "")

	l := NewLocal(LocalOptions{Log: discardLogger()})
	d := Details{Disks: []Disk{{Path: path, Format: "raw"}}}
	l.enrichDisks(&d)

	if d.Disks[0].Format != "raw" {
		t.Errorf("format = %q, want the raw the command line gave", d.Disks[0].Format)
	}
	if d.Disks[0].VirtualSize != 4<<30 {
		t.Errorf("virtual size = %d, want the header field", d.Disks[0].VirtualSize)
	}
}
