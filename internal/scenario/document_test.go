package scenario

import (
	"strings"
	"testing"
)

func TestParseDocumentOK(t *testing.T) {
	doc, err := ParseDocument([]byte(`{
		"apiVersion": "labview.dev/v1",
		"kind": "Scenario",
		"metadata": {"name": "smoke"},
		"spec": {
			"leaseAs": "harness/smoke/run-1",
			"machines": [{"use": "el9-build"}],
			"steps": [
				{"power": {"machine": "el9-build", "op": "start"}},
				{"waitSerial": {"machine": "el9-build", "match": "login:", "timeout": "5s"}},
				{"keys": {"machine": "el9-build", "sequence": ["ret", ["ctrl", "c"]]}},
				{"evidence": {"screenshot": ["el9-build"]}}
			],
			"cleanup": "keep"
		}
	}`))
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	if doc.Metadata.Name != "smoke" {
		t.Fatalf("name = %q", doc.Metadata.Name)
	}
	if len(doc.Spec.Steps) != 4 {
		t.Fatalf("steps = %d", len(doc.Spec.Steps))
	}
	if got := doc.Spec.Steps[2].Keys.Sequence[1].Keys; len(got) != 2 || got[0] != "ctrl" {
		t.Fatalf("chord = %#v", doc.Spec.Steps[2].Keys.Sequence)
	}
}

func TestParseDocumentRejectsUnknownField(t *testing.T) {
	_, err := ParseDocument([]byte(`{
		"apiVersion": "labview.dev/v1",
		"kind": "Scenario",
		"metadata": {"name": "x"},
		"spec": {
			"leaseAs": "harness/x/1",
			"machines": [{"use": "a"}],
			"steps": [{"power": {"machine": "a", "op": "start"}}]
		},
		"extra": true
	}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseDocumentIgnoresXPrefix(t *testing.T) {
	_, err := ParseDocument([]byte(`{
		"apiVersion": "labview.dev/v1",
		"kind": "Scenario",
		"metadata": {"name": "x"},
		"x-private": {"foo": 1},
		"spec": {
			"leaseAs": "harness/x/1",
			"machines": [{"use": "a"}],
			"steps": [{"power": {"machine": "a", "op": "start"}}]
		}
	}`))
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
}

func TestParseDocumentHotplug(t *testing.T) {
	doc, err := ParseDocument([]byte(`{
		"apiVersion": "labview.dev/v1",
		"kind": "Scenario",
		"metadata": {"name": "usb"},
		"spec": {
			"leaseAs": "harness/usb/run-1",
			"machines": [{"use": "el9-build"}],
			"steps": [
				{"hotplug": {"machine": "el9-build", "op": "add", "id": "stick0", "kind": "usb-storage", "image": "/tmp/stick.img"}},
				{"hotplug": {"machine": "el9-build", "op": "remove", "id": "stick0"}}
			],
			"cleanup": "keep"
		}
	}`))
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	if doc.Spec.Steps[0].Hotplug.Kind != "usb-storage" {
		t.Fatalf("kind = %q", doc.Spec.Steps[0].Hotplug.Kind)
	}
}

func TestParseDocumentRejectsBadHotplug(t *testing.T) {
	_, err := ParseDocument([]byte(`{
		"apiVersion": "labview.dev/v1",
		"kind": "Scenario",
		"metadata": {"name": "usb"},
		"spec": {
			"leaseAs": "harness/usb/run-1",
			"machines": [{"use": "el9-build"}],
			"steps": [
				{"hotplug": {"machine": "el9-build", "op": "add", "id": "stick0", "kind": "usb-storage"}}
			],
			"cleanup": "keep"
		}
	}`))
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseDocumentRejectsBadLease(t *testing.T) {
	_, err := ParseDocument([]byte(`{
		"apiVersion": "labview.dev/v1",
		"kind": "Scenario",
		"metadata": {"name": "x"},
		"spec": {
			"leaseAs": "alice",
			"machines": [{"use": "a"}],
			"steps": [{"power": {"machine": "a", "op": "start"}}]
		}
	}`))
	if err == nil || !strings.Contains(err.Error(), "leaseAs") {
		t.Fatalf("err = %v", err)
	}
}
