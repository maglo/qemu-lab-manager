package peer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShortNamesTheFirstResourceThatIsShort(t *testing.T) {
	free := Capacity{Lifecycle: true, KVM: true, CPUs: 4, MemoryFreeMB: 4096, DiskFreeGB: 50}
	cases := map[string]struct {
		c    Capacity
		n    Need
		want string
	}{
		"fits":       {free, Need{MemoryMB: 4096, VCPUs: 4, DiskGB: 50}, ""},
		"memory":     {free, Need{MemoryMB: 4097, VCPUs: 1}, "memory: 4096 MB free, the machine needs 4097 MB"},
		"cpu":        {free, Need{MemoryMB: 1, VCPUs: 5}, "CPU: 4 on the node, the machine needs 5"},
		"disk":       {free, Need{MemoryMB: 1, VCPUs: 1, DiskGB: 51}, "disk: 50 GB free, the machine needs 51 GB"},
		"no kvm":     {Capacity{Lifecycle: true}, Need{}, "no KVM"},
		"create off": {Capacity{KVM: true}, Need{}, "machine create is off"},
	}
	for name, tc := range cases {
		if got := tc.c.Short(tc.n); got != tc.want {
			t.Errorf("%s: Short = %q, want %q", name, got, tc.want)
		}
	}
}

func TestMemAvailableReadsTheKernelLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	doc := "MemTotal:       16000000 kB\nMemFree:          100000 kB\nMemAvailable:    8388608 kB\n"
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := memAvailableMB(path); got != 8192 {
		t.Fatalf("memAvailableMB = %d, want 8192", got)
	}
	if got := memAvailableMB(filepath.Join(t.TempDir(), "absent")); got != 0 {
		t.Fatalf("an absent file gave %d", got)
	}
}

func TestLocalCapacityOfThisHost(t *testing.T) {
	c := LocalCapacity(t.TempDir())
	if !c.Lifecycle || c.CPUs < 1 || c.MemoryFreeMB <= 0 || c.DiskFreeGB < 0 {
		t.Fatalf("capacity = %+v", c)
	}
	if LocalCapacity("").Lifecycle {
		t.Fatal("a node with no machines directory reports that it can create")
	}
}
