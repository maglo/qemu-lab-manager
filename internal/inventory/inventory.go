// Package inventory reads the machines that labview serves.
//
// The inventory is a directory, and one YAML file in it describes one
// machine. The files are written by something else entirely -- a playbook, a
// script, a person with an editor -- and read again as soon as one of them
// appears, changes or goes away. See design section 6 and section 13
// ("Inventory in").
package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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

// Machine is one file of the inventory directory.
//
// VNC and Serial are deliberately unexported in the wire sense: see View,
// which is what reaches the browser. Clients name a machine by ID only, so a
// developer cannot ask labview to dial an address the inventory did not give
// it (design section 6).
type Machine struct {
	// ID comes from the file name, not from the file. One machine is one
	// file, so the name on disk is the name in the API, and two machines
	// cannot claim the same id.
	ID string `yaml:"-"`

	Name   string `yaml:"name"`
	Host   string `yaml:"host"`
	VNC    string `yaml:"vnc"`
	Serial string `yaml:"serial"`
	Unit   string `yaml:"unit"`
	Notes  string `yaml:"notes"`

	// Entry is the file as written. The details tab shows it verbatim
	// (design section 8), which means round tripping through this struct is
	// not good enough -- a field labview does not know about must still be
	// displayed.
	Entry map[string]any `yaml:"-"`
}

// DisplayName falls back to the id, so a file that omits name still renders
// sensibly.
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

// Set is an immutable snapshot of the inventory, ordered by id.
type Set struct {
	machines []Machine
	byID     map[string]Machine
}

func newSet(machines []Machine) *Set {
	s := &Set{
		machines: machines,
		byID:     make(map[string]Machine, len(machines)),
	}
	for _, m := range machines {
		s.byID[m.ID] = m
	}
	return s
}

// Machines returns the entries in id order.
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

// ParseMachine decodes one machine file. The id comes from the file name, so
// the caller supplies it.
func ParseMachine(id string, data []byte) (Machine, error) {
	if err := validateID(id); err != nil {
		return Machine{}, err
	}

	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Machine{}, fmt.Errorf("the file is not valid YAML: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	value, err := normalise(doc)
	if err != nil {
		return Machine{}, err
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return Machine{}, fmt.Errorf("the file must hold a mapping of settings, not %T", value)
	}
	if _, present := entry["id"]; present {
		return Machine{}, errors.New("the file name gives the id, so the file must not set one")
	}

	// Decode twice: once into the map above, once into the struct, so the
	// details tab can show the file verbatim including settings labview does
	// not model. An unknown setting is not an error -- the producer may be
	// newer than labview.
	m := Machine{ID: id, Entry: entry}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Machine{}, err
	}
	if err := validate(m); err != nil {
		return Machine{}, err
	}
	return m, nil
}

// normalise converts a decoded YAML value into the types encoding/json
// writes. The details tab serialises the entry as JSON, so a mapping key that
// JSON cannot carry fails here, at load, rather than when a record is served.
func normalise(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			n, err := normalise(val)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			key, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("the setting name %v must be a string", k)
			}
			n, err := normalise(val)
			if err != nil {
				return nil, err
			}
			out[key] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			n, err := normalise(val)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	default:
		return v, nil
	}
}

func validateID(id string) error {
	if id == "" {
		return errors.New("id is required")
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("id %q must match %s", id, idPattern)
	}
	// Belt and braces: idPattern already excludes the separators, but an id
	// reaching the filesystem deserves an explicit check.
	if strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("id %q must not contain path separators", id)
	}
	return nil
}

func validate(m Machine) error {
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

// machineID maps a directory entry to the machine it describes. Both YAML
// suffixes count, because an operator who writes the other one deserves a
// machine rather than silence. Anything else in the directory is not ours:
// a README, an editor's backup, a dot file a writer has not finished with.
func machineID(name string) (string, bool) {
	if strings.HasPrefix(name, ".") {
		return "", false
	}
	ext := filepath.Ext(name)
	if ext != ".yaml" && ext != ".yml" {
		return "", false
	}
	return strings.TrimSuffix(name, ext), true
}

// FileError names the file that did not load. The name is safe to show; the
// message is not, because it can quote a value from the file and section 6
// keeps the addresses off every client.
type FileError struct {
	Name string
	Err  error
}

func (e *FileError) Error() string { return e.Name + ": " + e.Err.Error() }

func (e *FileError) Unwrap() error { return e.Err }

// fileState is what the loader remembers about one file between scans.
type fileState struct {
	mod  time.Time
	size int64

	// machine is the last version of this file that loaded. A file that
	// stops parsing keeps its machine live: a half-written file must not
	// stop a running broker.
	machine *Machine
	err     error
}

// loader scans the inventory directory and remembers what it read, so a scan
// that finds nothing new costs one stat per file.
type loader struct {
	dir   string
	files map[string]*fileState
}

func newLoader(dir string) *loader {
	return &loader{dir: dir, files: make(map[string]*fileState)}
}

// scan reads the directory and returns the set it describes. changed reports
// whether any file appeared, changed or went away since the previous scan.
// The errors name the files that did not load; the other files still do.
func (l *loader) scan() (set *Set, changed bool, errs []error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, false, []error{err}
	}

	machines := make([]Machine, 0, len(entries))
	owner := make(map[string]string, len(entries))
	seen := make(map[string]bool, len(entries))

	// os.ReadDir sorts by file name, and the file name is the id, so the
	// set comes out in id order.
	for _, entry := range entries {
		name := entry.Name()
		id, ok := machineID(name)
		if !ok {
			continue
		}
		path := filepath.Join(l.dir, name)
		info, err := os.Stat(path)
		if err != nil {
			errs = append(errs, &FileError{Name: name, Err: err})
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		seen[name] = true

		state := l.files[name]
		if state == nil || !state.mod.Equal(info.ModTime()) || state.size != info.Size() {
			changed = true
			state = l.read(path, name, id, info, state)
			l.files[name] = state
		}
		if state.err != nil {
			errs = append(errs, state.err)
		}
		if state.machine == nil {
			continue
		}
		if other, dup := owner[id]; dup {
			errs = append(errs, &FileError{Name: name,
				Err: fmt.Errorf("machine %q already comes from %s", id, other)})
			continue
		}
		owner[id] = name
		machines = append(machines, *state.machine)
	}

	for name := range l.files {
		if !seen[name] {
			delete(l.files, name)
			changed = true
		}
	}
	return newSet(machines), changed, errs
}

func (l *loader) read(path, name, id string, info os.FileInfo, previous *fileState) *fileState {
	state := &fileState{mod: info.ModTime(), size: info.Size()}
	data, err := os.ReadFile(path)
	if err == nil {
		var m Machine
		if m, err = ParseMachine(id, data); err == nil {
			state.machine = &m
			return state
		}
	}
	state.err = &FileError{Name: name, Err: err}
	if previous != nil {
		state.machine = previous.machine
	}
	return state
}

// LoadDir reads every machine file in dir. It is strict: one file that does
// not load fails the whole call, which is what a first load wants.
func LoadDir(dir string) (*Set, error) {
	set, _, errs := newLoader(dir).scan()
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return set, nil
}
