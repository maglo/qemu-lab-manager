// Package scenario parses and runs versioned Scenario documents
// (docs/design/scenarios.md).
package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/lifecycle"
)

// APIVersionV1 is the only major this package accepts.
const APIVersionV1 = "labview.dev/v1"

// KindScenario is the document kind for a run.
const KindScenario = "Scenario"

// Cleanup values.
const (
	CleanupDestroy = "destroy"
	CleanupKeep    = "keep"
)

var leaseAsPattern = regexp.MustCompile(`^harness/[^/]+/[^/]+$`)

// Document is a Scenario on the wire.
type Document struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

// Metadata names the scenario.
type Metadata struct {
	Name string `json:"name"`
}

// Spec is the runnable body.
type Spec struct {
	LeaseAs  string       `json:"leaseAs"`
	Machines []MachineRef `json:"machines"`
	Steps    []Step       `json:"steps"`
	Cleanup  string       `json:"cleanup"`
}

// MachineRef either reuses an id or creates through LCM.
type MachineRef struct {
	Use    string          `json:"use,omitempty"`
	Create *lifecycle.Spec `json:"create,omitempty"`
}

// Step is one action or assert. Exactly one field is set.
type Step struct {
	Power        *PowerStep        `json:"power,omitempty"`
	WaitSerial   *WaitSerialStep   `json:"waitSerial,omitempty"`
	Keys         *KeysStep         `json:"keys,omitempty"`
	AssertSerial *AssertSerialStep `json:"assertSerial,omitempty"`
	AssertScreen *AssertScreenStep `json:"assertScreen,omitempty"`
	Evidence     *EvidenceStep     `json:"evidence,omitempty"`
	HTTPGet      *HTTPGetStep      `json:"httpGet,omitempty"`
}

// PowerStep starts, stops or restarts a machine.
type PowerStep struct {
	Machine string `json:"machine"`
	Op      string `json:"op"`
}

// WaitSerialStep waits until serial contains Match.
type WaitSerialStep struct {
	Machine string `json:"machine"`
	Match   string `json:"match"`
	Timeout string `json:"timeout"`
}

// KeysStep sends chords in order.
type KeysStep struct {
	Machine  string     `json:"machine"`
	Sequence []KeyChord `json:"sequence"`
	HoldMs   int        `json:"holdMs,omitempty"`
}

// KeyChord is one chord. A string is one QEMU key name.
// A list is keys pressed together.
type KeyChord struct {
	Keys []string
}

// UnmarshalJSON accepts a string or a list of strings.
func (c *KeyChord) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		if one == "" {
			return errors.New("key chord is empty")
		}
		c.Keys = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("key chord must be a string or a list of strings")
	}
	if len(many) == 0 {
		return errors.New("key chord is empty")
	}
	c.Keys = many
	return nil
}

// MarshalJSON writes a string for one key, else a list.
func (c KeyChord) MarshalJSON() ([]byte, error) {
	if len(c.Keys) == 1 {
		return json.Marshal(c.Keys[0])
	}
	return json.Marshal(c.Keys)
}

// AssertSerialStep is waitSerial that fails the run on miss.
type AssertSerialStep = WaitSerialStep

// AssertScreenStep captures a screenshot (optional future image assert).
type AssertScreenStep struct {
	Machine string `json:"machine"`
	Capture bool   `json:"capture"`
}

// EvidenceStep copies captures into the run bundle.
type EvidenceStep struct {
	Screenshot []string `json:"screenshot,omitempty"`
	Serial     []string `json:"serial,omitempty"`
}

// HTTPGetStep GETs a URL the host can reach.
type HTTPGetStep struct {
	URL            string `json:"url"`
	WantStatus     int    `json:"wantStatus,omitempty"`
	Timeout        string `json:"timeout,omitempty"`
	BodyContains   string `json:"bodyContains,omitempty"`
	BodyNotContain string `json:"bodyNotContain,omitempty"`
}

// ParseDocument decodes and validates a Scenario JSON body.
func ParseDocument(data []byte) (Document, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Document{}, fmt.Errorf("body is not valid JSON: %w", err)
	}
	for k := range raw {
		if strings.HasPrefix(k, "x-") {
			delete(raw, k)
			continue
		}
		switch k {
		case "apiVersion", "kind", "metadata", "spec":
		default:
			return Document{}, fmt.Errorf("unknown field %q", k)
		}
	}
	trimmed, err := json.Marshal(raw)
	if err != nil {
		return Document{}, err
	}
	var doc Document
	dec := json.NewDecoder(strings.NewReader(string(trimmed)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("scenario document: %w", err)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Validate reports a document the runner must refuse.
func (d Document) Validate() error {
	var errs []error
	if d.APIVersion != APIVersionV1 {
		errs = append(errs, fmt.Errorf("apiVersion %q is not supported", d.APIVersion))
	}
	if d.Kind != KindScenario {
		errs = append(errs, fmt.Errorf("kind %q must be %s", d.Kind, KindScenario))
	}
	if d.Metadata.Name == "" {
		errs = append(errs, errors.New("metadata.name is required"))
	}
	if !leaseAsPattern.MatchString(d.Spec.LeaseAs) {
		errs = append(errs, errors.New("spec.leaseAs must match harness/<name>/<run-id>"))
	}
	if len(d.Spec.Machines) == 0 {
		errs = append(errs, errors.New("spec.machines must name at least one machine"))
	}
	for i, m := range d.Spec.Machines {
		switch {
		case m.Use != "" && m.Create != nil:
			errs = append(errs, fmt.Errorf("machines[%d] sets both use and create", i))
		case m.Use == "" && m.Create == nil:
			errs = append(errs, fmt.Errorf("machines[%d] needs use or create", i))
		case m.Create != nil:
			if err := m.Create.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("machines[%d].create: %w", i, err))
			}
		}
	}
	switch d.Spec.Cleanup {
	case CleanupDestroy, CleanupKeep, "":
	default:
		errs = append(errs, fmt.Errorf("spec.cleanup %q must be destroy or keep", d.Spec.Cleanup))
	}
	if len(d.Spec.Steps) == 0 {
		errs = append(errs, errors.New("spec.steps must name at least one step"))
	}
	for i, step := range d.Spec.Steps {
		if err := step.validate(); err != nil {
			errs = append(errs, fmt.Errorf("steps[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func (s Step) validate() error {
	n := 0
	if s.Power != nil {
		n++
		if s.Power.Machine == "" {
			return errors.New("power.machine is required")
		}
		switch s.Power.Op {
		case "start", "stop", "restart":
		default:
			return fmt.Errorf("power.op %q must be start, stop or restart", s.Power.Op)
		}
	}
	if s.WaitSerial != nil {
		n++
		if err := validateMatch(s.WaitSerial); err != nil {
			return err
		}
	}
	if s.Keys != nil {
		n++
		if s.Keys.Machine == "" {
			return errors.New("keys.machine is required")
		}
		if len(s.Keys.Sequence) == 0 {
			return errors.New("keys.sequence is required")
		}
	}
	if s.AssertSerial != nil {
		n++
		if err := validateMatch(s.AssertSerial); err != nil {
			return err
		}
	}
	if s.AssertScreen != nil {
		n++
		if s.AssertScreen.Machine == "" {
			return errors.New("assertScreen.machine is required")
		}
	}
	if s.Evidence != nil {
		n++
		if len(s.Evidence.Screenshot) == 0 && len(s.Evidence.Serial) == 0 {
			return errors.New("evidence needs screenshot or serial")
		}
	}
	if s.HTTPGet != nil {
		n++
		if s.HTTPGet.URL == "" {
			return errors.New("httpGet.url is required")
		}
	}
	if n != 1 {
		return errors.New("step must set exactly one action")
	}
	return nil
}

func validateMatch(w *WaitSerialStep) error {
	if w.Machine == "" {
		return errors.New("machine is required")
	}
	if w.Match == "" {
		return errors.New("match is required")
	}
	if _, err := ParseDuration(w.Timeout, 30*time.Second); err != nil {
		return err
	}
	return nil
}

// ParseDuration accepts Go durations. Empty uses the default.
func ParseDuration(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("timeout %q: %w", s, err)
	}
	if d <= 0 {
		return 0, errors.New("timeout must be positive")
	}
	return d, nil
}

// CleanupOrDefault returns destroy when unset.
func (s Spec) CleanupOrDefault() string {
	if s.Cleanup == "" {
		return CleanupDestroy
	}
	return s.Cleanup
}

// Name returns the step kind for telemetry.
func (s Step) Name() string {
	switch {
	case s.Power != nil:
		return "power"
	case s.WaitSerial != nil:
		return "waitSerial"
	case s.Keys != nil:
		return "keys"
	case s.AssertSerial != nil:
		return "assertSerial"
	case s.AssertScreen != nil:
		return "assertScreen"
	case s.Evidence != nil:
		return "evidence"
	case s.HTTPGet != nil:
		return "httpGet"
	}
	return "unknown"
}
