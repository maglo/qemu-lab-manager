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
	"github.com/maglo/qemu-lab-manager/labview/internal/lifecycle"
	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
	"github.com/maglo/qemu-lab-manager/labview/internal/qmp"
	"github.com/maglo/qemu-lab-manager/labview/internal/scenario"
	"github.com/maglo/qemu-lab-manager/labview/internal/serial"
	"github.com/maglo/qemu-lab-manager/labview/internal/telemetry"
)

// Server wires the pieces together and serves them.
type Server struct {
	cfg       config.Config
	inventory *inventory.Watcher
	brokers   *serial.Manager
	leases    *lease.Manager
	hosts     host.Access
	control   *qmp.Manager
	life      *lifecycle.Manager
	peers     *peer.Client
	activity  *activity.Log
	telemetry *telemetry.Bus
	scenarios *scenario.Runner
	peerFails *peerFailTracker
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
	// Control runs QMP commands on the machines that have a control
	// socket. Nil builds one from the configuration.
	Control *qmp.Manager
	// Life creates and destroys machines. Nil disables those endpoints.
	Life *lifecycle.Manager
	// Peers reaches other labview nodes (docs/design/cluster.md section 5).
	Peers *peer.Client

	Activity *activity.Log
	// Telemetry is the cluster event bus (docs/design/cluster.md section 6).
	// Nil disables GET /api/telemetry.
	Telemetry *telemetry.Bus
	// Scenarios runs versioned Scenario documents
	// (docs/design/scenarios.md). Nil disables the run endpoints.
	Scenarios *scenario.Runner
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
	control := d.Control
	if control == nil {
		control = qmp.NewManager(qmp.Options{
			CaptureDir: d.Config.CaptureDir,
			Timeout:    d.Config.ControlTimeout,
		})
	}
	if d.Activity != nil && d.Telemetry != nil {
		d.Activity.SetObserver(d.Telemetry)
	}
	s := &Server{
		cfg:       d.Config,
		inventory: d.Inventory,
		brokers:   d.Brokers,
		leases:    d.Leases,
		hosts:     d.Hosts,
		control:   control,
		life:      d.Life,
		peers:     d.Peers,
		activity:  d.Activity,
		telemetry: d.Telemetry,
		scenarios: d.Scenarios,
		peerFails: newPeerFailTracker(d.Telemetry),
		log:       log,
		mux:       http.NewServeMux(),
		ui:        d.UI,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// The section 5 surface. Machine-scoped HTTP forwards to a peer when
	// the id is composite (docs/design/cluster.md section 5).
	s.mux.HandleFunc("GET /api/machines", s.handleMachines)
	s.mux.HandleFunc("POST /api/machines", s.handleCreateMachine)
	s.mux.HandleFunc("GET /api/machines/{id}", s.remoteHTTP(s.handleMachine))
	s.mux.HandleFunc("DELETE /api/machines/{id}", s.remoteHTTP(s.handleDestroyMachine))
	s.mux.HandleFunc("POST /api/machines/{id}/lease", s.remoteHTTP(s.handleLease))
	s.mux.HandleFunc("GET /ws/console/{id}", s.handleConsoleWS)
	s.mux.HandleFunc("GET /ws/serial/{id}", s.handleSerialWS)

	// The tabs of section 8, each also reachable as JSON.
	s.mux.HandleFunc("POST /api/machines/{id}/power", s.remoteHTTP(s.handlePower))
	s.mux.HandleFunc("POST /api/machines/{id}/keys", s.remoteHTTP(s.handleKeys))
	s.mux.HandleFunc("GET /api/machines/{id}/screenshot", s.remoteHTTP(s.handleScreenshot))
	s.mux.HandleFunc("GET /api/machines/{id}/recordings", s.remoteHTTP(s.handleRecordings))
	s.mux.HandleFunc("GET /api/machines/{id}/recordings/{name}", s.remoteHTTP(s.handleRecording))
	s.mux.HandleFunc("GET /api/machines/{id}/activity", s.remoteHTTP(s.handleActivity))
	s.mux.HandleFunc("GET /api/activity", s.handleAllActivity)
	s.mux.HandleFunc("GET /api/telemetry", s.handleTelemetry)
	s.mux.HandleFunc("GET /api/scenarios/runs", s.handleScenarioRuns)
	s.mux.HandleFunc("POST /api/scenarios/runs", s.handleScenarioRunCreate)
	s.mux.HandleFunc("GET /api/scenarios/runs/{id}", s.handleScenarioRun)
	s.mux.HandleFunc("POST /api/scenarios/runs/{id}/cancel", s.handleScenarioRunCancel)
	s.mux.HandleFunc("GET /api/scenarios/runs/{id}/evidence/{name}", s.handleScenarioEvidence)
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	// A path under the API surface is never a route of the browser
	// application, so it answers as the API answers rather than falling
	// through to index.html.
	s.mux.HandleFunc("/api/", s.handleNoEndpoint)
	s.mux.HandleFunc("/ws/", s.handleNoEndpoint)

	if s.ui != nil {
		// Registered for every method, because a pattern that names one
		// method cannot sit under the prefix patterns above: the mux
		// refuses the pair as ambiguous. The UI handler answers the
		// methods it serves.
		s.mux.Handle("/", s.ui)
	}
}

// handleNoEndpoint answers a request for an endpoint that does not exist.
//
// A client cannot tell a removed endpoint from a working one if the answer is
// an HTML page with a 200, so this says what happened in the shape every other
// error has.
func (s *Server) handleNoEndpoint(w http.ResponseWriter, r *http.Request) {
	// The mux answers 405 only while nothing else matches, and this matches
	// everything under the prefix, so the method check lands here now.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if method == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := s.mux.Handler(probe); pattern != "" && !strings.HasSuffix(pattern, "/") {
			w.Header().Set("Allow", method)
			writeError(w, http.StatusMethodNotAllowed,
				"%s is not allowed on %q", r.Method, r.URL.Path)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no endpoint %q", r.URL.Path)
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

// remoteHTTP forwards a machine-scoped request to a peer when the path id
// is composite. Local ids fall through to the wrapped handler.
func (s *Server) remoteHTTP(local http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.peers != nil && s.peers.ProxyMachine(w, r, s.identity(r)) {
			return
		}
		local(w, r)
	}
}

// rejectRemoteWS refuses a websocket for a peer machine until forwarding
// lands in a later slice.
func (s *Server) rejectRemoteWS(w http.ResponseWriter, r *http.Request) bool {
	id := r.PathValue("id")
	if _, _, ok := peer.SplitID(id); !ok {
		return false
	}
	writeError(w, http.StatusNotImplemented,
		"websocket forwarding for remote machine %q is not available yet", id)
	return true
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
