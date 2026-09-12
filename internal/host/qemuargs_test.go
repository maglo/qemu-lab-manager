package host

import "testing"

// A realistic invocation of the kind the lab's playbook produces.
var realisticArgs = []string{
	"/usr/libexec/qemu-kvm",
	"-name", "guest=el9-build,debug-threads=on",
	"-machine", "pc-q35-rhel9.2.0,usb=off,dump-guest-core=off",
	"-cpu", "host,migratable=on",
	"-m", "4096",
	"-smp", "4,sockets=4,cores=1,threads=1",
	"-drive", "file=/var/lib/libvirt/images/el9-build.qcow2,format=qcow2,if=virtio,cache=none",
	"-drive", "file=/var/lib/libvirt/images/el9-data.qcow2,format=qcow2,if=virtio,readonly=on",
	"-drive", "file=/usr/share/OVMF/OVMF_CODE.fd,if=pflash,format=raw,readonly=on",
	"-netdev", "tap,id=net0,ifname=tap-el9,br=br-lab,script=no,downscript=no",
	"-device", "virtio-net-pci,netdev=net0,id=nic0,mac=52:54:00:12:34:56",
	"-vnc", "10.20.0.11:1",
	"-chardev", "socket,id=serial0,path=/run/qemu/el9-build-serial.sock,server=on,wait=off",
	"-serial", "chardev:serial0",
}

func TestParseQEMUArgsHardware(t *testing.T) {
	hw, _, _ := ParseQEMUArgs(realisticArgs)
	if hw.MemoryMB != 4096 {
		t.Errorf("MemoryMB = %d, want 4096", hw.MemoryMB)
	}
	if hw.VCPUs != 4 {
		t.Errorf("VCPUs = %d, want 4", hw.VCPUs)
	}
	if hw.MachineType != "pc-q35-rhel9.2.0" {
		t.Errorf("MachineType = %q", hw.MachineType)
	}
	if hw.CPUModel != "host" {
		t.Errorf("CPUModel = %q, want host", hw.CPUModel)
	}
	// pflash is firmware, not a data disk.
	if hw.Firmware != "/usr/share/OVMF/OVMF_CODE.fd" {
		t.Errorf("Firmware = %q", hw.Firmware)
	}
}

func TestParseQEMUArgsDisks(t *testing.T) {
	_, disks, _ := ParseQEMUArgs(realisticArgs)
	if len(disks) != 2 {
		t.Fatalf("got %d disks, want 2 (pflash is firmware, not a disk): %+v", len(disks), disks)
	}
	if disks[0].Path != "/var/lib/libvirt/images/el9-build.qcow2" {
		t.Errorf("disk 0 path = %q", disks[0].Path)
	}
	if disks[0].Format != "qcow2" || disks[0].Interface != "virtio" {
		t.Errorf("disk 0 = %+v", disks[0])
	}
	if disks[0].ReadOnly {
		t.Error("disk 0 should be writable")
	}
	if !disks[1].ReadOnly {
		t.Error("disk 1 should be read only")
	}
}

// The tap lives on -netdev and the MAC on the -device that refers to it by
// id, so the parser has to join them.
func TestParseQEMUArgsJoinsNetdevAndDevice(t *testing.T) {
	_, _, nics := ParseQEMUArgs(realisticArgs)
	if len(nics) != 1 {
		t.Fatalf("got %d NICs, want 1: %+v", len(nics), nics)
	}
	n := nics[0]
	if n.Tap != "tap-el9" {
		t.Errorf("Tap = %q", n.Tap)
	}
	if n.Bridge != "br-lab" {
		t.Errorf("Bridge = %q", n.Bridge)
	}
	if n.MAC != "52:54:00:12:34:56" {
		t.Errorf("MAC = %q", n.MAC)
	}
	if n.Model != "virtio-net-pci" {
		t.Errorf("Model = %q", n.Model)
	}
	if n.NetdevID != "net0" {
		t.Errorf("NetdevID = %q", n.NetdevID)
	}
}

func TestParseMemorySpellings(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want int64
	}{
		{"4096", 4096},
		{"4G", 4096},
		{"4g", 4096},
		{"4GB", 4096},
		{"2048M", 2048},
		{"size=8G", 8192},
		{"size=2048,slots=4,maxmem=16G", 2048},
		{"1T", 1024 * 1024},
		{"2097152K", 2048},
		{"", 0},
		{"garbage", 0},
	} {
		if got := parseMemoryMB(tc.arg); got != tc.want {
			t.Errorf("parseMemoryMB(%q) = %d, want %d", tc.arg, got, tc.want)
		}
	}
}

func TestParseSMPSpellings(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want int
	}{
		{"4", 4},
		{"cpus=8", 8},
		{"8,sockets=2,cores=4,threads=1", 8},
		{"sockets=2,cores=4,threads=1", 8},
		{"sockets=1,cores=2", 2},
		{"", 0},
	} {
		if got := parseSMP(tc.arg); got != tc.want {
			t.Errorf("parseSMP(%q) = %d, want %d", tc.arg, got, tc.want)
		}
	}
}

func TestParseQEMUArgsBareDriveAndCdrom(t *testing.T) {
	_, disks, _ := ParseQEMUArgs([]string{
		"qemu",
		"-drive", "/images/plain.raw",
		"-cdrom", "/images/boot.iso",
	})
	if len(disks) != 2 {
		t.Fatalf("got %d disks: %+v", len(disks), disks)
	}
	if disks[0].Path != "/images/plain.raw" {
		t.Errorf("bare -drive path = %q", disks[0].Path)
	}
	if disks[1].Interface != "cdrom" || !disks[1].ReadOnly {
		t.Errorf("cdrom = %+v", disks[1])
	}
}

func TestParseQEMUArgsStandaloneNic(t *testing.T) {
	_, _, nics := ParseQEMUArgs([]string{
		"qemu", "-nic", "tap,ifname=tap9,br=br0,mac=52:54:00:aa:bb:cc,model=virtio-net-pci",
	})
	if len(nics) != 1 {
		t.Fatalf("got %d NICs", len(nics))
	}
	if nics[0].MAC != "52:54:00:aa:bb:cc" || nics[0].Tap != "tap9" {
		t.Errorf("nic = %+v", nics[0])
	}
}

func TestParseQEMUArgsBiosFirmware(t *testing.T) {
	hw, _, _ := ParseQEMUArgs([]string{"qemu", "-bios", "/usr/share/seabios/bios.bin"})
	if hw.Firmware != "/usr/share/seabios/bios.bin" {
		t.Errorf("Firmware = %q", hw.Firmware)
	}
}

// Malformed and truncated command lines must not panic: the command line
// comes from /proc and labview does not control its shape.
func TestParseQEMUArgsToleratesGarbage(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"qemu", "-m"},             // flag with no value
		{"qemu", "-drive"},         // ditto
		{"qemu", "-drive", ""},     // empty value
		{"qemu", "-netdev", "tap"}, // netdev with no id
		{"qemu", "-device", "virtio-net-pci,netdev=missing"},
		{"qemu", "-smp", "-m", "-machine"},
		{"-m", "4096"},
	}
	for i, args := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("case %d panicked: %v", i, r)
				}
			}()
			ParseQEMUArgs(args)
		}()
	}
}

// A -device naming a netdev that does not exist must not invent a NIC.
func TestParseQEMUArgsIgnoresDanglingDevice(t *testing.T) {
	_, _, nics := ParseQEMUArgs([]string{
		"qemu", "-device", "virtio-net-pci,netdev=nope,mac=52:54:00:00:00:01",
	})
	if len(nics) != 0 {
		t.Fatalf("invented %d NICs from a dangling device: %+v", len(nics), nics)
	}
}

func TestKVLookup(t *testing.T) {
	v := "tap,id=net0,ifname=tap-el9,br=br-lab"
	if got := kvLookup(v, "ifname"); got != "tap-el9" {
		t.Errorf("ifname = %q", got)
	}
	if got := kvLookup(v, "absent"); got != "" {
		t.Errorf("absent = %q, want empty", got)
	}
	// A key that is a prefix of another must not match it.
	if got := kvLookup("id=net0,identity=x", "id"); got != "net0" {
		t.Errorf("id = %q, want net0", got)
	}
}

func TestFirstFieldSkipsKeyValue(t *testing.T) {
	if got := firstField("tap,id=net0"); got != "tap" {
		t.Errorf("firstField = %q", got)
	}
	// A list that starts with a key=value has no positional type.
	if got := firstField("id=net0,ifname=tap0"); got != "" {
		t.Errorf("firstField = %q, want empty", got)
	}
}
