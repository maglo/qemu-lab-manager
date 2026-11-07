package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/host"
	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
	"github.com/maglo/qemu-lab-manager/labview/internal/lifecycle"
	"github.com/maglo/qemu-lab-manager/labview/internal/qmp"
	"github.com/maglo/qemu-lab-manager/labview/internal/serial"
	"github.com/maglo/qemu-lab-manager/labview/internal/telemetry"
)

// Run status values.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusPassed    = "passed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Deps are the runner's collaborators.
type Deps struct {
	Inventory *inventory.Watcher
	Brokers   *serial.Manager
	Leases    *lease.Manager
	Hosts     host.Access
	Control   *qmp.Manager
	Life      *lifecycle.Manager
	Telemetry *telemetry.Bus
	// Dir holds evidence bundles. Empty keeps evidence only in memory.
	Dir string
	Log *slog.Logger
	// HTTPClient is used by httpGet. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// Now is injectable for tests.
	Now func() time.Time
}

// Runner accepts documents and drives runs.
type Runner struct {
	deps Deps

	mu   sync.Mutex
	runs map[string]*Run
}

// New returns a Runner.
func New(d Deps) *Runner {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Runner{deps: d, runs: make(map[string]*Run)}
}

// Run is one execution of a Scenario document.
type Run struct {
	ID         string       `json:"id"`
	Status     string       `json:"status"`
	Name       string       `json:"name"`
	APIVersion string       `json:"apiVersion"`
	LeaseAs    string       `json:"leaseAs"`
	Step       int          `json:"step"`
	StepName   string       `json:"stepName,omitempty"`
	Error      string       `json:"error,omitempty"`
	StartedAt  time.Time    `json:"startedAt"`
	FinishedAt time.Time    `json:"finishedAt,omitzero"`
	Steps      []StepResult `json:"steps,omitempty"`
	Evidence   []string     `json:"evidence,omitempty"`

	doc     Document
	created []string
	cancel  context.CancelFunc
	dir     string
	mu      sync.Mutex
}

// StepResult is the outcome of one step.
type StepResult struct {
	Index   int       `json:"index"`
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Error   string    `json:"error,omitempty"`
	Started time.Time `json:"startedAt"`
	Ended   time.Time `json:"finishedAt,omitzero"`
}

// Start validates doc, registers a run and executes it in the background.
func (r *Runner) Start(ctx context.Context, doc Document) (*Run, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	id := runIDFromLease(doc.Spec.LeaseAs)
	now := r.deps.Now().UTC()

	run := &Run{
		ID:         id,
		Status:     StatusPending,
		Name:       doc.Metadata.Name,
		APIVersion: doc.APIVersion,
		LeaseAs:    doc.Spec.LeaseAs,
		StartedAt:  now,
		doc:        doc,
	}
	runCtx, cancel := context.WithCancel(ctx)
	run.cancel = cancel

	r.mu.Lock()
	if _, exists := r.runs[id]; exists {
		r.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("run %q already exists", id)
	}
	r.runs[id] = run
	r.mu.Unlock()

	if r.deps.Dir != "" {
		dir := filepath.Join(r.deps.Dir, id)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			r.failRegister(run, cancel, err)
			return nil, err
		}
		run.dir = dir
		raw, _ := json.MarshalIndent(doc, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "scenario.json"), raw, 0o640)
	}

	go r.execute(runCtx, run)
	return run.snapshot(), nil
}

func (r *Runner) failRegister(run *Run, cancel context.CancelFunc, err error) {
	cancel()
	r.mu.Lock()
	delete(r.runs, run.ID)
	r.mu.Unlock()
	_ = err
}

// Get returns a copy of a run.
func (r *Runner) Get(id string) (*Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[id]
	if !ok {
		return nil, false
	}
	return run.snapshot(), true
}

// List returns runs, newest first.
func (r *Runner) List() []*Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Run, 0, len(r.runs))
	for _, run := range r.runs {
		out = append(out, run.snapshot())
	}
	// Newest first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].StartedAt.After(out[i].StartedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Cancel asks a run to stop. Cleanup still runs.
func (r *Runner) Cancel(id string) (*Run, error) {
	r.mu.Lock()
	run, ok := r.runs[id]
	r.mu.Unlock()
	if !ok {
		return nil, errNotFound
	}
	run.mu.Lock()
	if run.Status == StatusPending || run.Status == StatusRunning {
		if run.cancel != nil {
			run.cancel()
		}
	}
	run.mu.Unlock()
	return run.snapshot(), nil
}

// EvidenceFile returns a file from the run bundle.
func (r *Runner) EvidenceFile(id, name string) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return "", errBadEvidence
	}
	r.mu.Lock()
	run, ok := r.runs[id]
	r.mu.Unlock()
	if !ok {
		return "", errNotFound
	}
	run.mu.Lock()
	dir := run.dir
	run.mu.Unlock()
	if dir == "" {
		return "", errNoEvidence
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		return "", errNotFound
	}
	return path, nil
}

var (
	errNotFound    = errors.New("run not found")
	errBadEvidence = errors.New("bad evidence name")
	errNoEvidence  = errors.New("run has no evidence directory")
)

// ErrNotFound reports an unknown run id.
func ErrNotFound() error { return errNotFound }

// IsNotFound reports errNotFound.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }

func runIDFromLease(leaseAs string) string {
	// harness/<name>/<run-id>
	parts := strings.Split(leaseAs, "/")
	if len(parts) == 3 && parts[2] != "" {
		return parts[2]
	}
	return fmt.Sprintf("run-%d", time.Now().UnixNano())
}

func (run *Run) snapshot() *Run {
	run.mu.Lock()
	defer run.mu.Unlock()
	cp := *run
	cp.Steps = append([]StepResult(nil), run.Steps...)
	cp.Evidence = append([]string(nil), run.Evidence...)
	cp.created = append([]string(nil), run.created...)
	cp.cancel = nil
	cp.doc = Document{}
	return &cp
}

func (r *Runner) execute(ctx context.Context, run *Run) {
	r.setStatus(run, StatusRunning, "")
	r.emit(telemetry.Event{
		Kind: telemetry.KindScenario,
		Type: telemetry.TypeRunStarted,
		User: run.LeaseAs,
		Fields: map[string]any{
			"runId":      run.ID,
			"apiVersion": run.APIVersion,
			"name":       run.Name,
		},
	})

	err := r.prepareMachines(ctx, run)
	if err == nil {
		err = r.runSteps(ctx, run)
	}
	cleanupErr := r.cleanup(ctx, run)

	final := StatusPassed
	msg := ""
	if ctx.Err() != nil {
		final = StatusCancelled
		msg = "cancelled"
	} else if err != nil {
		final = StatusFailed
		msg = err.Error()
	}
	if cleanupErr != nil && msg == "" {
		final = StatusFailed
		msg = cleanupErr.Error()
	} else if cleanupErr != nil {
		msg = msg + "; cleanup: " + cleanupErr.Error()
	}

	r.finish(run, final, msg)
	r.writeResult(run)
	r.emit(telemetry.Event{
		Kind:   telemetry.KindScenario,
		Type:   telemetry.TypeRunFinished,
		User:   run.LeaseAs,
		Detail: final,
		Fields: map[string]any{
			"runId":      run.ID,
			"apiVersion": run.APIVersion,
			"name":       run.Name,
			"status":     final,
		},
	})
}

func (r *Runner) prepareMachines(ctx context.Context, run *Run) error {
	for _, ref := range run.doc.Spec.Machines {
		id := ref.Use
		if ref.Create != nil {
			if r.deps.Life == nil || !r.deps.Life.Enabled() {
				return errors.New("create requires lifecycle (machines-dir)")
			}
			mach, err := r.deps.Life.Create(ctx, *ref.Create)
			if err != nil {
				return fmt.Errorf("create %s: %w", ref.Create.ID, err)
			}
			id = mach.ID
			run.mu.Lock()
			run.created = append(run.created, id)
			run.mu.Unlock()
		}
		if _, ok := r.deps.Inventory.Current().Get(id); !ok {
			return fmt.Errorf("machine %q is not in the inventory", id)
		}
		if _, err := r.deps.Leases.Acquire(id, run.LeaseAs); err != nil {
			return fmt.Errorf("lease %s: %w", id, err)
		}
	}
	go r.renewLoop(ctx, run)
	return nil
}

func (r *Runner) renewLoop(ctx context.Context, run *Run) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, id := range r.machineIDs(run) {
				_, _ = r.deps.Leases.Renew(id, run.LeaseAs)
			}
		}
	}
}

func (r *Runner) machineIDs(run *Run) []string {
	ids := make([]string, 0, len(run.doc.Spec.Machines))
	for _, ref := range run.doc.Spec.Machines {
		if ref.Use != "" {
			ids = append(ids, ref.Use)
		} else if ref.Create != nil {
			ids = append(ids, ref.Create.ID)
		}
	}
	return ids
}

func (r *Runner) runSteps(ctx context.Context, run *Run) error {
	for i, step := range run.doc.Spec.Steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := step.Name()
		r.setStep(run, i, name)
		r.emit(telemetry.Event{
			Kind: telemetry.KindScenario,
			Type: telemetry.TypeStepStarted,
			User: run.LeaseAs,
			Fields: map[string]any{
				"runId": run.ID,
				"name":  run.Name,
				"step":  name,
				"index": i,
			},
		})
		started := r.deps.Now().UTC()
		err := r.runStep(ctx, run, step)
		ended := r.deps.Now().UTC()
		sr := StepResult{Index: i, Name: name, Started: started, Ended: ended}
		if err != nil {
			sr.Status = StatusFailed
			sr.Error = err.Error()
			run.mu.Lock()
			run.Steps = append(run.Steps, sr)
			run.mu.Unlock()
			r.emit(telemetry.Event{
				Kind:   telemetry.KindScenario,
				Type:   telemetry.TypeStepFinished,
				User:   run.LeaseAs,
				Detail: StatusFailed,
				Fields: map[string]any{
					"runId": run.ID, "name": run.Name, "step": name, "index": i, "status": StatusFailed,
				},
			})
			return err
		}
		sr.Status = StatusPassed
		run.mu.Lock()
		run.Steps = append(run.Steps, sr)
		run.mu.Unlock()
		r.emit(telemetry.Event{
			Kind:   telemetry.KindScenario,
			Type:   telemetry.TypeStepFinished,
			User:   run.LeaseAs,
			Detail: StatusPassed,
			Fields: map[string]any{
				"runId": run.ID, "name": run.Name, "step": name, "index": i, "status": StatusPassed,
			},
		})
	}
	return nil
}

func (r *Runner) runStep(ctx context.Context, run *Run, step Step) error {
	switch {
	case step.Power != nil:
		return r.stepPower(ctx, run, step.Power)
	case step.WaitSerial != nil:
		return r.stepWaitSerial(ctx, step.WaitSerial, false)
	case step.AssertSerial != nil:
		return r.stepWaitSerial(ctx, step.AssertSerial, true)
	case step.Keys != nil:
		return r.stepKeys(ctx, run, step.Keys)
	case step.AssertScreen != nil:
		return r.stepAssertScreen(ctx, run, step.AssertScreen)
	case step.Evidence != nil:
		return r.stepEvidence(ctx, run, step.Evidence)
	case step.HTTPGet != nil:
		return r.stepHTTPGet(ctx, step.HTTPGet)
	default:
		return errors.New("empty step")
	}
}

func (r *Runner) stepPower(ctx context.Context, run *Run, step *PowerStep) error {
	m, err := r.machine(step.Machine)
	if err != nil {
		return err
	}
	if !r.deps.Leases.CanWrite(m.ID, run.LeaseAs) {
		return fmt.Errorf("no write lease on %s", m.ID)
	}
	if !m.CanPower() {
		return fmt.Errorf("machine %q has no unit", m.ID)
	}
	return r.deps.Hosts.Power(ctx, m, host.Op(step.Op))
}

func (r *Runner) stepKeys(ctx context.Context, run *Run, step *KeysStep) error {
	m, err := r.machine(step.Machine)
	if err != nil {
		return err
	}
	if !m.HasControl() {
		return fmt.Errorf("machine %q has no control socket", m.ID)
	}
	hold := step.HoldMs
	if hold <= 0 {
		hold = 100
	}
	t := qmp.Target{ID: m.ID, Network: m.ControlNetwork(), Address: m.Control}
	for _, chord := range step.Sequence {
		if err := qmp.ValidateKeys(chord.Keys); err != nil {
			return err
		}
		if !r.deps.Leases.Touch(m.ID, run.LeaseAs) {
			return fmt.Errorf("no write lease on %s", m.ID)
		}
		if err := r.deps.Control.SendKeys(ctx, t, chord.Keys, hold); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) stepWaitSerial(ctx context.Context, step *WaitSerialStep, assert bool) error {
	timeout, err := ParseDuration(step.Timeout, 30*time.Second)
	if err != nil {
		return err
	}
	broker, ok := r.deps.Brokers.Get(step.Machine)
	if !ok {
		return fmt.Errorf("no serial broker for %q", step.Machine)
	}
	sub, scrollback, err := broker.Attach()
	if err != nil {
		return err
	}
	defer sub.Close()

	var buf bytes.Buffer
	buf.Write(scrollback)
	if bytes.Contains(buf.Bytes(), []byte(step.Match)) {
		return nil
	}

	deadline := time.After(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			kind := "waitSerial"
			if assert {
				kind = "assertSerial"
			}
			return fmt.Errorf("%s: match %q not seen within %s", kind, step.Match, timeout)
		case ev, ok := <-sub.Events():
			if !ok {
				return errors.New("serial subscriber closed")
			}
			if ev.Kind != serial.EventOutput {
				continue
			}
			buf.Write(ev.Data)
			if bytes.Contains(buf.Bytes(), []byte(step.Match)) {
				return nil
			}
		}
	}
}

func (r *Runner) stepAssertScreen(ctx context.Context, run *Run, step *AssertScreenStep) error {
	m, err := r.machine(step.Machine)
	if err != nil {
		return err
	}
	if !m.HasControl() {
		return fmt.Errorf("machine %q has no control socket", m.ID)
	}
	png, err := r.deps.Control.Screenshot(ctx, qmp.Target{
		ID: m.ID, Network: m.ControlNetwork(), Address: m.Control,
	})
	if err != nil {
		return err
	}
	if step.Capture || run.dir != "" {
		name := fmt.Sprintf("screen-%s.png", m.ID)
		if err := r.writeEvidence(run, name, png); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) stepEvidence(ctx context.Context, run *Run, step *EvidenceStep) error {
	for _, id := range step.Screenshot {
		m, err := r.machine(id)
		if err != nil {
			return err
		}
		if !m.HasControl() {
			return fmt.Errorf("machine %q has no control socket", id)
		}
		png, err := r.deps.Control.Screenshot(ctx, qmp.Target{
			ID: m.ID, Network: m.ControlNetwork(), Address: m.Control,
		})
		if err != nil {
			return err
		}
		if err := r.writeEvidence(run, fmt.Sprintf("screen-%s.png", id), png); err != nil {
			return err
		}
	}
	for _, id := range step.Serial {
		broker, ok := r.deps.Brokers.Get(id)
		if !ok {
			return fmt.Errorf("no serial broker for %q", id)
		}
		state := broker.State()
		if state.Transcript == "" {
			// Fall back to scrollback bytes as a text capture.
			data := broker.Scrollback()
			if err := r.writeEvidence(run, fmt.Sprintf("serial-%s.txt", id), data); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(state.Transcript)
		if err != nil {
			return err
		}
		if err := r.writeEvidence(run, fmt.Sprintf("serial-%s.cast", id), data); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) stepHTTPGet(ctx context.Context, step *HTTPGetStep) error {
	timeout, err := ParseDuration(step.Timeout, 10*time.Second)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, step.URL, nil)
	if err != nil {
		return err
	}
	client := r.deps.HTTPClient
	c := *client
	c.Timeout = timeout
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	want := step.WantStatus
	if want == 0 {
		want = http.StatusOK
	}
	if resp.StatusCode != want {
		return fmt.Errorf("httpGet: status %d, want %d", resp.StatusCode, want)
	}
	if step.BodyContains != "" && !bytes.Contains(body, []byte(step.BodyContains)) {
		return fmt.Errorf("httpGet: body does not contain %q", step.BodyContains)
	}
	if step.BodyNotContain != "" && bytes.Contains(body, []byte(step.BodyNotContain)) {
		return fmt.Errorf("httpGet: body contains %q", step.BodyNotContain)
	}
	return nil
}

func (r *Runner) cleanup(ctx context.Context, run *Run) error {
	ids := r.machineIDs(run)
	var errs []error
	if run.doc.Spec.CleanupOrDefault() == CleanupDestroy {
		run.mu.Lock()
		created := append([]string(nil), run.created...)
		run.mu.Unlock()
		for _, id := range created {
			if r.deps.Life == nil {
				errs = append(errs, fmt.Errorf("destroy %s: lifecycle disabled", id))
				continue
			}
			if err := r.deps.Life.Destroy(ctx, id); err != nil {
				errs = append(errs, fmt.Errorf("destroy %s: %w", id, err))
			}
		}
	}
	for _, id := range ids {
		_ = r.deps.Leases.Release(id, run.LeaseAs)
	}
	return errors.Join(errs...)
}

func (r *Runner) machine(id string) (inventory.Machine, error) {
	m, ok := r.deps.Inventory.Current().Get(id)
	if !ok {
		return inventory.Machine{}, fmt.Errorf("machine %q is not in the inventory", id)
	}
	return m, nil
}

func (r *Runner) writeEvidence(run *Run, name string, data []byte) error {
	run.mu.Lock()
	dir := run.dir
	run.mu.Unlock()
	if dir == "" {
		// Still record the name so the run shows evidence was requested.
		run.mu.Lock()
		run.Evidence = append(run.Evidence, name)
		run.mu.Unlock()
		return nil
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return err
	}
	run.mu.Lock()
	run.Evidence = append(run.Evidence, name)
	run.mu.Unlock()
	return nil
}

func (r *Runner) writeResult(run *Run) {
	snap := run.snapshot()
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil || run.dir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(run.dir, "result.json"), raw, 0o640)
}

func (r *Runner) setStatus(run *Run, status, msg string) {
	run.mu.Lock()
	run.Status = status
	if msg != "" {
		run.Error = msg
	}
	run.mu.Unlock()
}

func (r *Runner) setStep(run *Run, i int, name string) {
	run.mu.Lock()
	run.Step = i
	run.StepName = name
	run.mu.Unlock()
}

func (r *Runner) finish(run *Run, status, msg string) {
	run.mu.Lock()
	run.Status = status
	run.Error = msg
	run.FinishedAt = r.deps.Now().UTC()
	run.mu.Unlock()
}

func (r *Runner) emit(ev telemetry.Event) {
	if r.deps.Telemetry != nil {
		r.deps.Telemetry.Emit(ev)
	}
}
