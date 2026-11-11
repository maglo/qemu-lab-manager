package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWaitsForPass(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/scenarios/runs":
			posts++
			if got := r.Header.Get("X-Forwarded-User"); got != "harness/cli/run-1" {
				t.Errorf("identity = %q", got)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"run-1","status":"running"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/scenarios/runs/run-1":
			_, _ = w.Write([]byte(`{"id":"run-1","status":"passed"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	doc := map[string]any{
		"apiVersion": "labview.dev/v1",
		"kind":       "Scenario",
		"metadata":   map[string]string{"name": "cli"},
		"spec": map[string]any{
			"leaseAs":  "harness/cli/run-1",
			"machines": []any{map[string]string{"use": "m"}},
			"steps":    []any{map[string]any{"power": map[string]string{"machine": "m", "op": "start"}}},
			"cleanup":  "keep",
		},
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := cmdRun([]string{"-f", path, "-base", srv.URL, "-poll", "1ms"}); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("posts = %d", posts)
	}
}

func TestGetRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"run-1","status":"passed"}`))
	}))
	defer srv.Close()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := cmdGet([]string{"-base", srv.URL, "run", "run-1"})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"passed"`) {
		t.Fatalf("out = %s", out)
	}
}

func TestEvidence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("PNGDATA"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "screen.png")
	if err := cmdEvidence([]string{"-base", srv.URL, "-o", out, "run-1", "screen-a.png"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "PNGDATA" {
		t.Fatalf("got %q", got)
	}
}
