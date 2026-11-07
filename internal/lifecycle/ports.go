package lifecycle

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
)

const (
	vncBasePort   = 5900
	vncDisplayMin = 1
	vncDisplayMax = 99
)

// allocateVNCPort picks a display number in 1..99 that no managed machine
// already uses. The inventory stores host:port with port = 5900+display.
func allocateVNCPort(machinesDir, id string) (display int, err error) {
	used := map[int]struct{}{}
	entries, err := os.ReadDir(machinesDir)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == id {
			continue
		}
		port, ok := readStoredVNCPort(filepath.Join(machinesDir, e.Name(), "spec.json"))
		if !ok {
			continue
		}
		display := port - vncBasePort
		if display >= vncDisplayMin && display <= vncDisplayMax {
			used[display] = struct{}{}
		}
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	start := int(h.Sum32()%uint32(vncDisplayMax-vncDisplayMin+1)) + vncDisplayMin
	for i := 0; i < vncDisplayMax-vncDisplayMin+1; i++ {
		d := vncDisplayMin + (start-vncDisplayMin+i)%(vncDisplayMax-vncDisplayMin+1)
		if _, taken := used[d]; !taken {
			return d, nil
		}
	}
	return 0, fmt.Errorf("no free VNC display in %d..%d", vncDisplayMin, vncDisplayMax)
}

func readStoredVNCPort(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var s storedSpec
	if err := json.Unmarshal(b, &s); err != nil {
		return 0, false
	}
	if s.VNCPort <= 0 {
		return 0, false
	}
	return s.VNCPort, true
}
