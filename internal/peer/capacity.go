package peer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// CapacityPath is where a node reports its capacity.
const CapacityPath = "/api/cluster/capacity"

// Capacity is what a node has free for a new machine
// (docs/design/cluster.md section 5).
type Capacity struct {
	// Lifecycle is true when the node can create a machine.
	Lifecycle    bool  `json:"lifecycle"`
	KVM          bool  `json:"kvm"`
	CPUs         int   `json:"cpus"`
	MemoryFreeMB int64 `json:"memoryFreeMb"`
	DiskFreeGB   int64 `json:"diskFreeGb"`
}

// Need is what a new machine takes from a node.
type Need struct {
	MemoryMB int64
	VCPUs    int
	DiskGB   int64
}

// Short says why the node cannot take the machine. It is empty when the
// machine fits.
func (c Capacity) Short(n Need) string {
	switch {
	case !c.Lifecycle:
		return "machine create is off"
	case !c.KVM:
		return "no KVM"
	case c.MemoryFreeMB < n.MemoryMB:
		return fmt.Sprintf("memory: %d MB free, the machine needs %d MB", c.MemoryFreeMB, n.MemoryMB)
	case c.CPUs < n.VCPUs:
		return fmt.Sprintf("CPU: %d on the node, the machine needs %d", c.CPUs, n.VCPUs)
	case c.DiskFreeGB < n.DiskGB:
		return fmt.Sprintf("disk: %d GB free, the machine needs %d GB", c.DiskFreeGB, n.DiskGB)
	}
	return ""
}

// LocalCapacity reads the capacity of this host. machinesDir is where the
// disks of a new machine go. An empty machinesDir means that create is off.
func LocalCapacity(machinesDir string) Capacity {
	c := Capacity{
		Lifecycle:    machinesDir != "",
		KVM:          KVMPresent(),
		CPUs:         runtime.NumCPU(),
		MemoryFreeMB: memAvailableMB("/proc/meminfo"),
	}
	var st syscall.Statfs_t
	if machinesDir != "" && syscall.Statfs(machinesDir, &st) == nil {
		c.DiskFreeGB = int64(st.Bavail) * int64(st.Bsize) >> 30
	}
	return c
}

// memAvailableMB reads MemAvailable, which counts the memory that the
// kernel can take back from its caches. It is 0 when the file has no
// such line.
func memAvailableMB(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "MemAvailable:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return kb >> 10
	}
	return 0
}

// Capacity asks a peer for its capacity.
func (c *Client) Capacity(ctx context.Context, p Peer) (Capacity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+CapacityPath, nil)
	if err != nil {
		return Capacity{}, err
	}
	c.setHeaders(req, "")
	res, err := c.http.Do(req)
	if err != nil {
		return Capacity{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return Capacity{}, err
	}
	if res.StatusCode != http.StatusOK {
		return Capacity{}, fmt.Errorf("status %d: %s", res.StatusCode, truncate(string(body), 200))
	}
	var out Capacity
	if err := json.Unmarshal(body, &out); err != nil {
		return Capacity{}, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

// Create sends a create document to a peer. It returns the status and
// the body of the answer. The id in a created machine is the composite id.
func (c *Client) Create(ctx context.Context, p Peer, identity string, doc []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+"/api/machines", bytes.NewReader(doc))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setHeaders(req, identity)
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	if res.StatusCode != http.StatusCreated {
		return res.StatusCode, body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return 0, nil, fmt.Errorf("decode: %w", err)
	}
	if localID, _ := m["id"].(string); localID != "" {
		m["id"] = JoinID(p.Name, localID)
	}
	if host, _ := m["host"].(string); host == "" {
		m["host"] = p.Name
	}
	out, err := json.Marshal(m)
	return res.StatusCode, out, err
}
