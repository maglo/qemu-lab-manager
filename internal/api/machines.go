package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/host"
	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
	"github.com/maglo/qemu-lab-manager/labview/internal/serial"
)

// listingTimeout bounds host lookups on the machines listing, which the wall
// polls. A machine whose unit state cannot be had in time reports its serial
// liveness instead of holding up every other tile.
const listingTimeout = 2 * time.Second

// machineSummary is one row of the machines listing: the inventory entry plus
// liveness and the current lease holder (design section 5).
type machineSummary struct {
	inventory.View

	Serial *serialState `json:"serial,omitempty"`
	Unit   *host.Unit   `json:"unit,omitempty"`
	Lease  *lease.Lease `json:"lease,omitempty"`

	// Live is the single flag a tile needs: the machine is there and worth
	// showing. It is deliberately derived here rather than in the browser,
	// so the UI and a harness agree on what "up" means.
	Live bool `json:"live"`
	// Why explains a machine that is not live, since the wall shows
	// machines without a console and says why (design section 7).
	Why string `json:"why,omitempty"`
}

// serialState is the browser-facing subset of the broker's upstream state.
//
// The broker's last error is deliberately not in it. A dial or read failure
// names the endpoint it failed on ("dial unix /run/qemu/...: no such file"),
// and error text from an arbitrary source cannot be reliably scrubbed of an
// address, so it stays in labview's own log and the client gets the fact of
// the failure instead.
type serialState struct {
	Connected   bool      `json:"connected"`
	Since       time.Time `json:"since,omitzero"`
	Subscribers int       `json:"subscribers"`
	Scrollback  int64     `json:"scrollbackBytes"`
	Failing     bool      `json:"failing,omitempty"`
}

func (s *Server) summarise(ctx context.Context, m inventory.Machine, states map[string]serial.UpstreamState, leases map[string]lease.Lease) machineSummary {
	sum := machineSummary{View: m.View()}

	if st, ok := states[m.ID]; ok {
		sum.Serial = &serialState{
			Connected:   st.Connected,
			Since:       st.Since,
			Subscribers: st.Subscribers,
			Scrollback:  st.BytesReceived,
			Failing:     !st.Connected && st.LastError != "",
		}
	}
	if l, ok := leases[m.ID]; ok {
		lv := l
		sum.Lease = &lv
	}

	// Liveness, in order of authority: what systemd says, then whether the
	// serial line is answering, then nothing to go on.
	switch {
	case m.CanPower():
		if u, err := s.hosts.UnitState(ctx, m); err == nil {
			sum.Unit = &u
			sum.Live = u.Running()
			if !sum.Live {
				sum.Why = unitWhy(u)
			}
			return sum
		}
		fallthrough
	default:
		if sum.Serial != nil {
			sum.Live = sum.Serial.Connected
			if !sum.Live {
				sum.Why = "serial line is not answering"
			}
			return sum
		}
		if !m.HasConsole() && !m.HasSerial() {
			sum.Why = "no console and no serial line in the inventory"
			return sum
		}
		// A framebuffer-only machine with no unit: labview has nothing to
		// check short of dialling VNC, which the browser is about to do
		// anyway.
		sum.Live = true
		return sum
	}
}

func unitWhy(u host.Unit) string {
	switch {
	case u.LoadState == "not-found":
		return "systemd does not know this unit"
	case u.Failed():
		return "unit failed: " + strings.TrimSpace(u.SubState+" "+u.Result)
	case u.ActiveState != "":
		return "unit is " + u.ActiveState
	}
	return "unit state unknown"
}

// handleMachines serves the inventory plus liveness and lease holders.
func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	states := s.brokers.States()
	leases := s.leases.All()

	// The wall polls this, so it must not be at the mercy of a slow bus:
	// a machine whose unit state times out falls back to serial liveness.
	ctx, cancel := context.WithTimeout(r.Context(), listingTimeout)
	defer cancel()

	set := s.inventory.Current()
	out := make([]machineSummary, 0, set.Len())
	for _, m := range set.Machines() {
		out = append(out, s.summarise(ctx, m, states, leases))
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": out})
}

// machineRecord is the whole record for one machine: everything the details
// tab shows, and nothing the UI can see that a harness cannot (design
// section 8).
type machineRecord struct {
	machineSummary

	Details host.Details `json:"details"`

	// Inventory is the machine's entry in the inventory file, verbatim,
	// minus the fields that never reach a browser.
	Inventory map[string]any `json:"inventory"`

	Recordings []serial.Recording `json:"recordings"`
	Activity   []activity.Event   `json:"activity"`
}

// handleMachine serves one machine's whole record.
func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}

	rec := machineRecord{
		machineSummary: s.summarise(r.Context(), m, s.brokers.States(), s.leases.All()),
		Inventory:      redactedEntry(m),
		Recordings:     []serial.Recording{},
		Activity:       s.activity.Machine(m.ID),
	}

	details, err := s.hosts.Inspect(r.Context(), m)
	if err != nil {
		// Inspect is meant to degrade rather than fail, so an error here is
		// worth surfacing as a warning instead of losing the whole record.
		details.Warnings = append(details.Warnings, "host inspection failed: "+err.Error())
	}
	rec.Details = details

	if recs, err := serial.ListRecordings(s.cfg.TranscriptDir, m.ID); err == nil {
		rec.Recordings = recs
	}

	writeJSON(w, http.StatusOK, rec)
}

// redactedEntry returns the inventory entry as written, with the two address
// fields section 6 names replaced by a marker.
//
// The unit name is *not* withheld: section 8 lists it among the things the
// details tab shows. Section 12's rule is that a client never *names* a unit
// in a request, which is about the request path -- labview maps an id to a
// unit itself and will only touch units the inventory gave it.
func redactedEntry(m inventory.Machine) map[string]any {
	entry := map[string]any{}
	if len(m.Raw) > 0 {
		if err := json.Unmarshal(m.Raw, &entry); err != nil {
			entry = map[string]any{}
		}
	}
	for _, address := range []string{"vnc", "serial"} {
		if _, present := entry[address]; present {
			// Say that something was withheld rather than silently
			// dropping it: a developer reading the details tab should not
			// wonder why the file has a field the page does not.
			entry[address] = "(withheld: labview does not send console addresses to clients)"
		}
	}
	return entry
}

// handleLogs serves recent journal lines for a machine's unit.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	n := intParam(r, "lines", s.cfg.LogLines, 1, 5000)

	lines, err := s.hosts.Logs(r.Context(), m, host.LogOptions{Lines: n})
	switch {
	case noUnit(err):
		writeError(w, http.StatusNotImplemented, "%s", err.Error())
		return
	case hostUnavailable(err):
		writeError(w, http.StatusNotImplemented, "%s", err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "reading the journal failed: %s", err.Error())
		return
	}
	if lines == nil {
		lines = []host.LogLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// handleRecordings lists a machine's past serial captures.
func (s *Server) handleRecordings(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	if s.cfg.TranscriptDir == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"recordings": []serial.Recording{},
			"note":       "serial capture is disabled on this labview",
		})
		return
	}
	recs, err := serial.ListRecordings(s.cfg.TranscriptDir, m.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing recordings failed: %s", err.Error())
		return
	}
	if recs == nil {
		recs = []serial.Recording{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"recordings": recs})
}

// handleRecording serves one capture for playback or download.
func (s *Server) handleRecording(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	if s.cfg.TranscriptDir == "" {
		writeError(w, http.StatusNotFound, "serial capture is disabled on this labview")
		return
	}

	// The name comes from a client, so it is validated rather than trusted:
	// it must be a plain filename belonging to this machine.
	name := r.PathValue("name")
	if !serial.RecordingBelongsTo(name, m.ID) {
		writeError(w, http.StatusBadRequest, "not a recording name for machine %q", m.ID)
		return
	}

	path := filepath.Join(s.cfg.TranscriptDir, name)
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such recording")
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		writeError(w, http.StatusNotFound, "no such recording")
		return
	}

	// asciicast is JSON lines. Served as a download by default so a browser
	// does not try to render a large capture as text.
	w.Header().Set("Content-Type", "application/x-asciicast")
	w.Header().Set("Cache-Control", "no-store")
	if !boolParam(r, "inline") {
		w.Header().Set("Content-Disposition", "attachment; filename="+name)
	}
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// handleActivity serves one machine's history.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	m, ok := s.machine(w, r)
	if !ok {
		return
	}
	events := s.activity.Machine(m.ID)
	if events == nil {
		events = []activity.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"activity": events})
}

// handleAllActivity serves the whole lab's history.
func (s *Server) handleAllActivity(w http.ResponseWriter, r *http.Request) {
	events := s.activity.Recent()
	if events == nil {
		events = []activity.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"activity": events})
}

// handleConfig tells the UI the few things it cannot infer: which tile
// renderer to use, and who the proxy says it is.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tileMode":         string(s.cfg.TileMode),
		"identity":         s.identity(r),
		"leaseIdleSeconds": int(s.cfg.LeaseIdle.Seconds()),
		"recordings":       s.cfg.TranscriptDir != "",
		"hostAccess":       string(s.cfg.HostAccess),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"machines": s.inventory.Current().Len(),
	})
}
