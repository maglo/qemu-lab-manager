package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/scenario"
)

func TestScenarioRunPowerStep(t *testing.T) {
	h := newHarness(t)
	evDir := filepath.Join(h.dir, "scenarios")
	h.srv.scenarios = scenario.New(scenario.Deps{
		Inventory: h.srv.inventory,
		Brokers:   h.brokers,
		Leases:    h.leases,
		Hosts:     h.fakeHost,
		Control:   h.srv.control,
		Telemetry: h.telem,
		Dir:       evDir,
		Log:       h.srv.log,
	})

	body := json.RawMessage(`{
		"apiVersion": "labview.dev/v1",
		"kind": "Scenario",
		"metadata": {"name": "power-smoke"},
		"spec": {
			"leaseAs": "harness/power-smoke/run-power-1",
			"machines": [{"use": "el9-build"}],
			"steps": [
				{"power": {"machine": "el9-build", "op": "start"}}
			],
			"cleanup": "keep"
		}
	}`)
	rec := h.do(http.MethodPost, "/api/scenarios/runs", "harness/power-smoke/run-power-1", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	var run scenario.Run
	for time.Now().Before(deadline) {
		rec = h.do(http.MethodGet, "/api/scenarios/runs/run-power-1", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("get status %d", rec.Code)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		if run.Status == scenario.StatusPassed || run.Status == scenario.StatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != scenario.StatusPassed {
		t.Fatalf("run status = %s error = %q", run.Status, run.Error)
	}
	ops := h.fakeHost.Ops()
	if len(ops) != 1 || ops[0] != "el9-build:start" {
		t.Fatalf("ops = %#v", ops)
	}

	cfg := h.do(http.MethodGet, "/api/config", "", "")
	var conf map[string]any
	if err := json.Unmarshal(cfg.Body.Bytes(), &conf); err != nil {
		t.Fatal(err)
	}
	if conf["scenarios"] != true {
		t.Fatalf("config.scenarios = %v", conf["scenarios"])
	}
}

func TestScenarioRunRejectsBadDocument(t *testing.T) {
	h := newHarness(t)
	h.srv.scenarios = scenario.New(scenario.Deps{
		Inventory: h.srv.inventory,
		Brokers:   h.brokers,
		Leases:    h.leases,
		Hosts:     h.fakeHost,
		Log:       h.srv.log,
	})
	rec := h.do(http.MethodPost, "/api/scenarios/runs", "u", json.RawMessage(`{"apiVersion":"labview.dev/v1","kind":"Scenario"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}
