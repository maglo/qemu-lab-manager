// Package inventory reads the machine list that labview serves.
//
// The file is written by something else entirely -- a playbook, a script, a
// person with an editor -- and re-read when its mtime changes. See design
// section 6 and section 13 ("Inventory in").
package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// idPattern constrains machine ids because an id is not just a label: it
// becomes a path element in the HTTP surface and a filename component in the
// transcript directory. Anything outside this set is rejected at load time
// rather than escaped at every use site.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// unitPattern constrains systemd unit names. Power operations go over D-Bus
// and never through a shell (design section 12), so this is defence in depth
// rather than quoting -- but the journal reader does exec journalctl, and a
// unit name is the one inventory field that reaches it.
var unitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._\\@-]*\.(service|target|socket|scope)$`)

// Machine is one entry of the inventory file.
//
// VNC and Serial are deliberately unexported in the wire sense: see View,
// which is what reaches the browser. Clients name a machine by ID only, so a
// developer cannot ask labview to dial an address the inventory did not give
// it (design section 6).
type Machine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	VNC    string `json:"vnc"`
	Serial string `json:"serial"`
	Unit   string `json:"unit"`
	Notes  string `json:"notes"`

	// Raw is the machine's entry exactly as it appeared in the file. The
	// details tab shows it verbatim (design section 8), which means round
	// tripping through this struct is not good enough -- a field labview
	// does not know about must still be displayed.
	Raw json.RawMessage `json:"-"`
}

// DisplayName falls back to the id, so an inventory that omits name still
// renders sensibly.
func (m Machine) DisplayName() string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// HasConsole reports whether this machine has a framebuffer worth showing.
// Machines without one get a serial tile instead (design section 7,
// "Serial-only").
func (m Machine) HasConsole() bool { return m.VNC != "" }

// HasSerial reports whether a serial broker should exist for this machine.
func (m Machine) HasSerial() bool { return m.Serial != "" }

// CanPower reports whether power operations are available. The inventory is
// the allowlist (design section 12): no unit in the file means labview has no
// business touching this machine's lifecycle, so the API refuses rather than
// guessing a unit name from the id.
func (m Machine) CanPower() bool { return m.Unit != "" }

// SerialNetwork classifies the serial address so the broker can net.Dial it.
// A leading slash is a unix socket path (labview alongside QEMU on the
// hypervisor); anything else is host:port. Either is a dial, so the broker
// does not care which (design section 6).
func (m Machine) SerialNetwork() string {
	if strings.HasPrefix(m.Serial, "/") {
		return "unix"
	}
	return "tcp"
}

// View is the browser-facing projection of a machine. It exists so that
// omitting vnc and serial is a property of the type rather than a discipline
// applied at each handler.
type View struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	Notes      string `json:"notes"`
	HasConsole bool   `json:"hasConsole"`
	HasSerial  bool   `json:"hasSerial"`
	CanPower   bool   `json:"canPower"`
}

// View projects the machine for the API. Note that Unit is also withheld: a
// client never names a unit (design section 12), so it has no use for it.
func (m Machine) View() View {
	return View{
		ID:         m.ID,
		Name:       m.DisplayName(),
		Host:       m.Host,
		Notes:      m.Notes,
		HasConsole: m.HasConsole(),
		HasSerial:  m.HasSerial(),
		CanPower:   m.CanPower(),
	}
}

// Set is an immutable snapshot of the inventory, ordered as the file was.
type Set struct {
	machines []Machine
	byID     map[string]Machine
}

// Machines returns the entries in file order.
func (s *Set) Machines() []Machine {
	if s == nil {
		return nil
	}
	out := make([]Machine, len(s.machines))
	copy(out, s.machines)
	return out
}

// Get looks a machine up by id.
func (s *Set) Get(id string) (Machine, bool) {
	if s == nil {
		return Machine{}, false
	}
	m, ok := s.byID[id]
	return m, ok
}

// Len reports how many machines the set holds.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.machines)
}

// Views projects the whole set for the API.
func (s *Set) Views() []View {
	out := make([]View, 0, s.Len())
	for _, m := range s.Machines() {
		out = append(out, m.View())
	}
	return out
}

// Parse decodes an inventory document.
//
// Validation is all-or-nothing on purpose. A partially applied inventory
// would mean brokers running against a file nobody wrote, so a malformed file
// leaves the previously loaded set in place instead.
func Parse(data []byte) (*Set, error) {
	// Decode twice: once into the struct, once into raw messages, so the
	// details tab can show an entry verbatim including fields labview does
	// not model.
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("inventory is not a JSON array: %w", err)
	}

	set := &Set{
		machines: make([]Machine, 0, len(raws)),
		byID:     make(map[string]Machine, len(raws)),
	}
	var errs []error
	for i, raw := range raws {
		var m Machine
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			// An unknown field is not fatal -- the producer may be newer
			// than labview -- so fall back to a lenient decode and keep
			// the raw entry for display.
			if err2 := json.Unmarshal(raw, &m); err2 != nil {
				errs = append(errs, fmt.Errorf("entry %d: %w", i, err2))
				continue
			}
		}
		m.Raw = raw

		if err := validate(m); err != nil {
			errs = append(errs, fmt.Errorf("entry %d: %w", i, err))
			continue
		}
		if _, dup := set.byID[m.ID]; dup {
			errs = append(errs, fmt.Errorf("entry %d: duplicate id %q", i, m.ID))
			continue
		}
		set.machines = append(set.machines, m)
		set.byID[m.ID] = m
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return set, nil
}

func validate(m Machine) error {
	if m.ID == "" {
		return errors.New("id is required")
	}
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("id %q must match %s", m.ID, idPattern)
	}
	// Belt and braces: idPattern already excludes the separators, but an id
	// reaching the filesystem deserves an explicit check.
	if strings.Contains(m.ID, "..") || strings.ContainsAny(m.ID, `/\`) {
		return fmt.Errorf("id %q must not contain path separators", m.ID)
	}
	if m.Unit != "" && !unitPattern.MatchString(m.Unit) {
		return fmt.Errorf("unit %q is not a valid systemd unit name", m.Unit)
	}
	if m.Serial != "" && m.SerialNetwork() == "tcp" && !strings.Contains(m.Serial, ":") {
		return fmt.Errorf("serial %q must be a unix path or host:port", m.Serial)
	}
	if m.VNC != "" && !strings.Contains(m.VNC, ":") {
		return fmt.Errorf("vnc %q must be host:port", m.VNC)
	}
	return nil
}

// Load reads and parses the inventory file.
func Load(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}
