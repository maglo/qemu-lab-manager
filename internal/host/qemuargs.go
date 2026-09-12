package host

import (
	"strconv"
	"strings"
)

// ParseQEMUArgs derives the hardware, disks and NICs shown on the details tab
// from a QEMU command line.
//
// The command line is the authority on what the VM actually is: it is what
// QEMU was invoked with, so it cannot drift from the running machine the way
// a separate config file would. Parsing it is best effort by nature -- QEMU
// has many spellings for the same thing -- so anything unrecognised is left
// out of the structured fields while the raw command line is still shown in
// full.
func ParseQEMUArgs(args []string) (Hardware, []Disk, []NIC) {
	var hw Hardware
	var disks []Disk

	// netdevs and devices are matched up at the end: a tap lives on
	// -netdev, while its MAC and model live on the -device that refers to
	// it by id.
	netdevs := map[string]NIC{}
	var netdevOrder []string
	macByNetdev := map[string]string{}
	modelByNetdev := map[string]string{}
	var standalone []NIC

	for i := 0; i < len(args); i++ {
		arg := args[i]
		val := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}

		switch arg {
		case "-m", "-memory":
			hw.MemoryMB = parseMemoryMB(val())
		case "-smp":
			hw.VCPUs = parseSMP(val())
		case "-machine", "-M":
			v := val()
			hw.MachineType = firstField(v)
			// Firmware is sometimes a machine property.
			if fw := kvLookup(v, "firmware"); fw != "" {
				hw.Firmware = fw
			}
			if pf := kvLookup(v, "pflash0"); pf != "" && hw.Firmware == "" {
				hw.Firmware = pf
			}
		case "-cpu":
			hw.CPUModel = firstField(val())
		case "-bios":
			hw.Firmware = val()
		case "-kernel":
			if hw.Firmware == "" {
				hw.Firmware = val()
			} else {
				val()
			}
		case "-drive":
			if d, ok := parseDrive(val()); ok {
				if d.Interface == "pflash" {
					// pflash is firmware, not a data disk. Showing OVMF
					// among the guest's disks is just noise.
					if hw.Firmware == "" {
						hw.Firmware = d.Path
					}
					continue
				}
				disks = append(disks, d)
			}
		case "-cdrom":
			disks = append(disks, Disk{Path: val(), Interface: "cdrom", ReadOnly: true})
		case "-netdev":
			if id, n, ok := parseNetdev(val()); ok {
				netdevs[id] = n
				netdevOrder = append(netdevOrder, id)
			}
		case "-nic":
			standalone = append(standalone, parseNic(val()))
		case "-device":
			v := val()
			if nd := kvLookup(v, "netdev"); nd != "" {
				if mac := kvLookup(v, "mac"); mac != "" {
					macByNetdev[nd] = mac
				}
				modelByNetdev[nd] = firstField(v)
				continue
			}
			// A disk can also arrive as -device ...,drive=id, but the
			// -drive it refers to has already been recorded.
		}
	}

	nics := make([]NIC, 0, len(netdevOrder)+len(standalone))
	for _, id := range netdevOrder {
		n := netdevs[id]
		n.NetdevID = id
		n.MAC = macByNetdev[id]
		n.Model = modelByNetdev[id]
		nics = append(nics, n)
	}
	nics = append(nics, standalone...)

	return hw, disks, nics
}

// parseMemoryMB accepts the spellings QEMU accepts: a bare number in MiB, a
// suffixed size, or size=N inside a property list.
func parseMemoryMB(v string) int64 {
	if v == "" {
		return 0
	}
	if s := kvLookup(v, "size"); s != "" {
		v = s
	} else {
		v = firstField(v)
	}
	if v == "" {
		return 0
	}

	// Strip a trailing byte marker first, so 4GB is read the same as 4G.
	v = strings.TrimSuffix(strings.TrimSuffix(v, "B"), "b")
	if v == "" {
		return 0
	}

	mult := int64(1) // bare numbers are MiB
	last := v[len(v)-1]
	switch last {
	case 'K', 'k':
		return parseInt(v[:len(v)-1]) / 1024
	case 'M', 'm':
		v = v[:len(v)-1]
	case 'G', 'g':
		mult, v = 1024, v[:len(v)-1]
	case 'T', 't':
		mult, v = 1024*1024, v[:len(v)-1]
	}
	return parseInt(v) * mult
}

func parseSMP(v string) int {
	if v == "" {
		return 0
	}
	if c := kvLookup(v, "cpus"); c != "" {
		return int(parseInt(c))
	}
	if n := parseInt(firstField(v)); n > 0 {
		return int(n)
	}
	// -smp sockets=2,cores=4,threads=1 with no explicit cpus.
	sockets := max64(parseInt(kvLookup(v, "sockets")), 1)
	cores := max64(parseInt(kvLookup(v, "cores")), 1)
	threads := max64(parseInt(kvLookup(v, "threads")), 1)
	if total := sockets * cores * threads; total > 1 {
		return int(total)
	}
	return 0
}

func parseDrive(v string) (Disk, bool) {
	if v == "" {
		return Disk{}, false
	}
	d := Disk{
		Path:      kvLookup(v, "file"),
		Format:    kvLookup(v, "format"),
		Interface: kvLookup(v, "if"),
	}
	if d.Path == "" {
		// -drive /path/to/img is accepted as a bare filename.
		if f := firstField(v); f != "" && !strings.Contains(f, "=") {
			d.Path = f
		}
	}
	if ro := kvLookup(v, "readonly"); ro == "on" || ro == "true" {
		d.ReadOnly = true
	}
	if d.Path == "" {
		return Disk{}, false
	}
	return d, true
}

func parseNetdev(v string) (string, NIC, bool) {
	if v == "" {
		return "", NIC{}, false
	}
	id := kvLookup(v, "id")
	if id == "" {
		return "", NIC{}, false
	}
	return id, NIC{
		Kind:   firstField(v),
		Tap:    kvLookup(v, "ifname"),
		Bridge: kvLookup(v, "br"),
	}, true
}

func parseNic(v string) NIC {
	return NIC{
		Kind:   firstField(v),
		Tap:    kvLookup(v, "ifname"),
		Bridge: kvLookup(v, "br"),
		MAC:    kvLookup(v, "mac"),
		Model:  kvLookup(v, "model"),
	}
}

// firstField returns the leading positional value of a comma separated QEMU
// property list, which is conventionally the type.
func firstField(v string) string {
	f := v
	if i := strings.IndexByte(f, ','); i >= 0 {
		f = f[:i]
	}
	if strings.Contains(f, "=") {
		return ""
	}
	return f
}

// kvLookup finds key=value in a comma separated QEMU property list.
func kvLookup(v, key string) string {
	for _, part := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(part, "=")
		if ok && k == key {
			return val
		}
	}
	return ""
}

func parseInt(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
