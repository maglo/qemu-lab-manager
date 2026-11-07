package lifecycle

import (
	"fmt"
	"os"
)

// createSparseDisk makes a raw disk image of sizeBytes without writing the
// data. QEMU accepts raw; this avoids execing qemu-img
// (docs/design/labview.md).
func createSparseDisk(path string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return fmt.Errorf("disk size must be positive")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("create disk: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(sizeBytes); err != nil {
		return fmt.Errorf("size disk: %w", err)
	}
	return nil
}
