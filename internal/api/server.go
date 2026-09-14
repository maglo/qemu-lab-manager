// Package api is labview's HTTP and websocket surface (design section 5).
//
// Everything the UI displays is also available as JSON: the UI is one
// consumer, a test harness is another, and neither gets a privileged path
// (design sections 8 and 13).
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/config"
	"github.com/maglo/qemu-lab-manager/labview/internal/host"
	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
	"github.com/maglo/qemu-lab-manager/labview/internal/serial"
)

// Server wires the pieces together and serves them.
type Server struct {
	cfg       config.Config
	inventory *inventory.Watcher
	brokers   *serial.Manager
	leases    *lease.Manager
	hosts     host.Access
	activity  *activity.Log
	log       *slog.Logger

	mux *http.ServeMux
	ui  http.Handler
}

// Deps are the Server's collaborators.
type Deps struct {
	Config    config.Config
	Inventory *inventory.Watcher
	Brokers   *serial.Manager
	Leases    *lease.Manager
	Hosts     host.Access
	Activity  *activity.Log
	Log       *slog.Logger

	// UI serves the browser application. Nil serves the API only, which is
	// all a harness needs.
	UI http.Handler
}

// New returns a Server with its routes registered.
func New(d Deps) *Server {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:       d.Config,
		inventory: d.Inventory,
		brokers:   d.Brokers,
		leases:    d.Leases,
		hosts:     d.Hosts,
		activity:  d.Activity,
		log:       log,
		mux:       http.NewServeMux(),
		ui:        d.UI,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// The section 5 surface.
	s.mux.HandleFunc("GET /api/machines", s.handleMachines)
	s.mux.HandleFunc("GET /api/machines/{id}", s.handleMachine)
	s.mux.HandleFunc("POST /api/machines/{id}/lease", s.handleLease)
	s.mux.HandleFunc("GET /ws/console/{id}", s.handleConsoleWS)
	s.mux.HandleFunc("GET /ws/serial/{id}", s.handleSerialWS)

	// The tabs of section 8, each also reachable as JSON.
	s.mux.HandleFunc("POST /api/machines/{id}/power", s.handlePower)
	s.mux.HandleFunc("GET /api/machines/{id}/recordings", s.handleRecordings)
	s.mux.HandleFunc("GET /api/machines/{id}/recordings/{name}", s.handleRecording)
	s.mux.HandleFunc("GET /api/machines/{id}/activity", s.handleActivity)
	s.mux.HandleFunc("GET /api/activity", s.handleAllActivity)
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	if s.ui != nil {
		s.mux.Handle("GET /", s.ui)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// identity returns the user the proxy asserted.
//
// It names the lease holder and says who attached to what. It is never an
// authorisation input: everyone who gets past the proxy can see every machine
// (design section 10).
func (s *Server) identity(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(s.cfg.IdentityHeader)); v != "" {
		return sanitiseIdentity(v)
	}
	return s.cfg.DefaultIdentity
}

// sanitiseIdentity keeps an asserted identity from carrying control
// characters into logs, transcripts and the UI. The proxy is trusted to say
// who someone is, not to say it tidily.
func sanitiseIdentity(v string) string {
	const maxLen = 128
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
	if len(v) > maxLen {
		v = v[:maxLen]
	}
	return v
}

// machine resolves the {id} path value against the live inventory.
//
// A client names a machine by id and nothing else, so this is the only way a
// request reaches an address or a unit (design sections 6 and 12).
func (s *Server) machine(w http.ResponseWriter, r *http.Request) (inventory.Machine, bool) {
	id := r.PathValue("id")
	m, ok := s.inventory.Current().Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no machine %q in the inventory", id)
		return inventory.Machine{}, false
	}
	return m, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		// The status line is already out; nothing useful left to do but
		// record it.
		slog.Default().Debug("response encode failed", "error", err)
	}
}

// errorBody is the shape of every error, so a harness can parse one.
type errorBody struct {
	Error string `json:"error"`
	// Holder is set when a request was refused because somebody else holds
	// the write lease, so a client can say who without a second request.
	Holder  string `json:"holder,omitempty"`
	Expires string `json:"expires,omitempty"`
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, errorBody{Error: fmt.Sprintf(format, args...)})
}

// intParam reads a bounded integer query parameter.
func intParam(r *http.Request, name string, def, minimum, maximum int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < minimum {
		return minimum
	}
	if n > maximum {
		return maximum
	}
	return n
}

// boolParam reads a flag-style query parameter. The serial endpoint's
// ?write=1 is the one that matters (design section 5).
func boolParam(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// hostUnavailable reports whether an error means host access is simply not
// configured, which is a 501 rather than a 500: nothing is broken, labview
// just is not running where it could answer.
func hostUnavailable(err error) bool {
	var unsupported *host.ErrUnsupported
	return errors.As(err, &unsupported)
}

func noUnit(err error) bool {
	var e *host.ErrNoUnit
	return errors.As(err, &e)
}
