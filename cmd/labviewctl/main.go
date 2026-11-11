// Command labviewctl is a thin HTTP client for scenario runs
// (docs/design/scenarios.md).
//
// It posts the same JSON documents the API accepts. It does not embed
// QEMU logic.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// version is overridable at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "labviewctl: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(usage())
		return nil
	case "version", "-version", "--version":
		fmt.Println(version)
		return nil
	case "run":
		return cmdRun(args[1:])
	case "get":
		return cmdGet(args[1:])
	case "evidence":
		return cmdEvidence(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage())
	}
}

func usage() string {
	return `labviewctl — drive scenario runs over the labview HTTP API

Usage:
  labviewctl run -f scenario.json [-base URL] [-wait] [-poll duration]
  labviewctl get run <id> [-base URL]
  labviewctl evidence <id> <name> [-base URL] [-o path]

-base defaults to http://127.0.0.1:8080
run sets X-Forwarded-User from spec.leaseAs in the document
`
}

type client struct {
	base     string
	identity string
	http     *http.Client
}

func newClient(base, identity string) *client {
	base = strings.TrimRight(base, "/")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return &client{
		base:     base,
		identity: identity,
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *client) do(method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.identity != "" {
		req.Header.Set("X-Forwarded-User", c.identity)
	}
	return c.http.Do(req)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("f", "", "scenario JSON file")
	base := fs.String("base", "http://127.0.0.1:8080", "labview base URL")
	wait := fs.Bool("wait", true, "wait until the run finishes")
	poll := fs.Duration("poll", time.Second, "status poll interval when -wait")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("run needs -f scenario.json")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var doc struct {
		Spec struct {
			LeaseAs string `json:"leaseAs"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("scenario JSON: %w", err)
	}
	c := newClient(*base, doc.Spec.LeaseAs)
	resp, err := c.do(http.MethodPost, "/api/scenarios/runs", raw)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /api/scenarios/runs: %s: %s", resp.Status, out)
	}
	var run map[string]any
	if err := json.Unmarshal(out, &run); err != nil {
		return err
	}
	id, _ := run["id"].(string)
	if !*wait {
		os.Stdout.Write(append(out, '\n'))
		return nil
	}
	for {
		status, body, err := c.getRun(id)
		if err != nil {
			return err
		}
		switch status {
		case "passed", "failed", "cancelled":
			os.Stdout.Write(append(body, '\n'))
			if status != "passed" {
				return fmt.Errorf("run %s %s", id, status)
			}
			return nil
		}
		time.Sleep(*poll)
	}
}

func (c *client) getRun(id string) (status string, body []byte, err error) {
	resp, err := c.do(http.MethodGet, "/api/scenarios/runs/"+id, nil)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", body, fmt.Errorf("GET run: %s: %s", resp.Status, body)
	}
	var run struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &run); err != nil {
		return "", body, err
	}
	return run.Status, body, nil
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("base", "http://127.0.0.1:8080", "labview base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 || rest[0] != "run" {
		return errors.New("usage: labviewctl get run <id>")
	}
	c := newClient(*base, "")
	_, body, err := c.getRun(rest[1])
	if err != nil {
		return err
	}
	os.Stdout.Write(append(body, '\n'))
	return nil
}

func cmdEvidence(args []string) error {
	fs := flag.NewFlagSet("evidence", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("base", "http://127.0.0.1:8080", "labview base URL")
	out := fs.String("o", "", "output file (default: basename of name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return errors.New("usage: labviewctl evidence <id> <name> [-o path]")
	}
	id, name := rest[0], rest[1]
	c := newClient(*base, "")
	resp, err := c.do(http.MethodGet, "/api/scenarios/runs/"+id+"/evidence/"+name, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET evidence: %s: %s", resp.Status, body)
	}
	path := *out
	if path == "" {
		path = filepath.Base(name)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}
