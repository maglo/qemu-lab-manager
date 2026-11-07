package telemetry

import (
	"syscall"
)

// DiskFreeBytes returns free bytes on the filesystem that holds path.
// An empty path or a Statfs error returns 0 and false.
func DiskFreeBytes(path string) (int64, bool) {
	if path == "" {
		return 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
