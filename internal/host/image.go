package host

import (
	"encoding/binary"
	"io"
	"os"
	"syscall"
)

// qcow2Magic opens every qcow2 image.
const qcow2Magic = "QFI\xfb"

// imageInfo holds what the details tab shows about one disk image.
type imageInfo struct {
	VirtualSize int64
	ActualSize  int64
	Format      string
	BackingFile string
}

// readImageInfo reads the sizes and the backing file of a disk image.
//
// The offsets are qcow2 header fields, all big-endian: the magic at 0,
// backing_file_offset at 8, backing_file_size at 16 and the virtual size at
// 24. A file without the magic is read as raw, where the virtual size is the
// length of the file.
//
// The actual size is st_blocks * 512, the number qemu-img reports for a file
// on disk. It counts this file alone, so an overlay reports what the overlay
// holds and names its backing file.
func readImageInfo(path string) (imageInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return imageInfo{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return imageInfo{}, err
	}
	info := imageInfo{ActualSize: blocksSize(st)}

	// 32 bytes carry every field this reads.
	var hdr [32]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[0:4]) != qcow2Magic {
		info.Format = "raw"
		info.VirtualSize = st.Size()
		return info, nil
	}

	info.Format = "qcow2"
	info.VirtualSize = int64(binary.BigEndian.Uint64(hdr[24:32]))
	info.BackingFile = readBackingFile(f, hdr)
	return info, nil
}

// readBackingFile returns the backing file name an image stores, or an empty
// string. Most images name none, so a name that does not read is not an
// error.
func readBackingFile(f *os.File, hdr [32]byte) string {
	off := binary.BigEndian.Uint64(hdr[8:16])
	size := binary.BigEndian.Uint32(hdr[16:20])
	// The specification caps the name at 1023 bytes.
	if off == 0 || size == 0 || size > 1023 {
		return ""
	}
	name := make([]byte, size)
	if _, err := f.ReadAt(name, int64(off)); err != nil {
		return ""
	}
	return string(name)
}

// blocksSize returns the space a file occupies on disk.
func blocksSize(fi os.FileInfo) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Blocks * 512
}
