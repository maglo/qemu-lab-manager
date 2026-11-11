package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
)

var (
	sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*"(?:,[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*")*\})? (\S+)$`)
	helpLine   = regexp.MustCompile(`^# HELP ([a-zA-Z_:][a-zA-Z0-9_:]*) \S.*$`)
	typeLine   = regexp.MustCompile(`^# TYPE ([a-zA-Z_:][a-zA-Z0-9_:]*) (gauge|counter)$`)
)

// scrape reads /metrics and checks each line against the Prometheus text
// format. It returns the samples, with the labels in the key.
func scrape(t *testing.T, h *harness) map[string]float64 {
	t.Helper()
	rec := h.do("GET", "/metrics", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(body, "\n") {
		t.Error("the last line has no line feed")
	}

	samples := make(map[string]float64)
	types := make(map[string]string)
	helped := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		switch {
		case helpLine.MatchString(line):
			name := helpLine.FindStringSubmatch(line)[1]
			if helped[name] {
				t.Errorf("%s has two HELP lines", name)
			}
			helped[name] = true
		case typeLine.MatchString(line):
			m := typeLine.FindStringSubmatch(line)
			if _, twice := types[m[1]]; twice {
				t.Errorf("%s has two TYPE lines", m[1])
			}
			types[m[1]] = m[2]
		case sampleLine.MatchString(line):
			m := sampleLine.FindStringSubmatch(line)
			if types[m[1]] == "" || !helped[m[1]] {
				t.Errorf("sample %q comes before its HELP and TYPE", line)
			}
			if types[m[1]] == "counter" && !strings.HasSuffix(m[1], "_total") {
				t.Errorf("counter %s does not end in _total", m[1])
			}
			v, err := strconv.ParseFloat(m[3], 64)
			if err != nil {
				t.Errorf("value in %q: %v", line, err)
			}
			if _, twice := samples[m[1]+m[2]]; twice {
				t.Errorf("sample %s%s is there two times", m[1], m[2])
			}
			samples[m[1]+m[2]] = v
		default:
			t.Errorf("line is not in the text format: %q", line)
		}
	}
	return samples
}

func TestMetricsReportTheNode(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Version = "1.2.3"
	h.srv.capacity = func() peer.Capacity {
		return peer.Capacity{Lifecycle: true, KVM: true, CPUs: 8, MemoryFreeMB: 2048, DiskFreeGB: 10}
	}

	got := scrape(t, h)
	for name, want := range map[string]float64{
		`labview_build_info{version="1.2.3"}`:           1,
		`labview_node_kvm`:                              1,
		`labview_node_lifecycle`:                        1,
		`labview_node_cpus`:                             8,
		`labview_node_memory_free_bytes`:                2048 << 20,
		`labview_node_disk_free_bytes`:                  10 << 30,
		`labview_node_peers`:                            0,
		`labview_node_machines`:                         3,
		`labview_machine_lease_held{machine="no-unit"}`: 0,
		`labview_lease_grants_total`:                    0,
		`labview_lease_denials_total`:                   0,
		`labview_peer_failures_total`:                   0,
	} {
		v, ok := got[name]
		if !ok {
			t.Errorf("%s is absent", name)
		} else if v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	for _, id := range []string{"el9-build", "serial-only", "no-unit"} {
		if _, ok := got[`labview_machine_live{machine="`+id+`"}`]; !ok {
			t.Errorf("no live sample for %s", id)
		}
	}
}

func TestMetricsCountLeasesAndDenials(t *testing.T) {
	h := newHarness(t)

	if rec := h.do("POST", "/api/machines/el9-build/lease", "alice", leaseRequest{Action: "acquire"}); rec.Code != http.StatusOK {
		t.Fatalf("acquire: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do("POST", "/api/machines/el9-build/lease", "bob", leaseRequest{Action: "acquire"}); rec.Code != http.StatusConflict {
		t.Fatalf("second acquire: %d %s", rec.Code, rec.Body)
	}

	got := scrape(t, h)
	if got["labview_lease_grants_total"] != 1 || got["labview_lease_denials_total"] != 1 {
		t.Errorf("grants = %v, denials = %v, want 1 and 1",
			got["labview_lease_grants_total"], got["labview_lease_denials_total"])
	}
	if got[`labview_machine_lease_held{machine="el9-build"}`] != 1 {
		t.Error("the held lease does not show")
	}
	if got[`labview_machine_lease_held{machine="serial-only"}`] != 0 {
		t.Error("a machine with no lease shows a held lease")
	}
}

func TestMetricsCountPeerFailures(t *testing.T) {
	h := clusterHarness(t)
	if _, err := h.srv.peers.Register(peer.Registration{Name: "gone", URL: "http://127.0.0.1:1", KVM: true}); err != nil {
		t.Fatal(err)
	}
	h.do("GET", "/api/machines", "alice", nil)
	h.do("GET", "/api/machines", "alice", nil)

	got := scrape(t, h)
	if got["labview_peer_failures_total"] != 2 {
		t.Errorf("peer failures = %v, want 2", got["labview_peer_failures_total"])
	}
	if got["labview_node_peers"] != 1 {
		t.Errorf("peers = %v, want 1", got["labview_node_peers"])
	}
}

func TestMetricsEscapeLabelValues(t *testing.T) {
	var m metricsWriter
	m.sample("x", 1, "machine", "a\"b\\c\nd")
	if got, want := m.buf.String(), "x{machine=\"a\\\"b\\\\c\\nd\"} 1\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
