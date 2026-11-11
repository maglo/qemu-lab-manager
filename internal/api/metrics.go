package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
)

// metricsContentType is the Prometheus text exposition format.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// metricsWriter writes the Prometheus text format. labview has few
// metrics, so it writes the format and takes no client library.
type metricsWriter struct {
	buf bytes.Buffer
}

func (m *metricsWriter) family(name, kind, help string) {
	fmt.Fprintf(&m.buf, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func (m *metricsWriter) sample(name string, value float64, labels ...string) {
	m.buf.WriteString(name)
	for i := 0; i+1 < len(labels); i += 2 {
		sep := ","
		if i == 0 {
			sep = "{"
		}
		fmt.Fprintf(&m.buf, `%s%s="%s"`, sep, labels[i], escapeLabel(labels[i+1]))
	}
	if len(labels) >= 2 {
		m.buf.WriteByte('}')
	}
	fmt.Fprintf(&m.buf, " %g\n", value)
}

func (m *metricsWriter) one(name, kind, help string, value float64, labels ...string) {
	m.family(name, kind, help)
	m.sample(name, value, labels...)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(v string) string { return labelEscaper.Replace(v) }

func flag(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// handleMetrics serves the numbers of this node (docs/design/cluster.md
// section 6). Each node serves its own, and Prometheus scrapes each node.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var m metricsWriter

	version := s.cfg.Version
	if version == "" {
		version = "dev"
	}
	m.one("labview_build_info", "gauge", "The version of labview. The value is 1.", 1, "version", version)

	// The gauges of the node are the numbers that placement uses.
	free := s.localCapacity()
	m.one("labview_node_kvm", "gauge", "1 when the node has KVM.", flag(free.KVM))
	m.one("labview_node_lifecycle", "gauge", "1 when the node can create a machine.", flag(free.Lifecycle))
	m.one("labview_node_cpus", "gauge", "The CPU count of the node.", float64(free.CPUs))
	m.one("labview_node_memory_free_bytes", "gauge",
		"The memory that a new machine can use.", float64(free.MemoryFreeMB)*(1<<20))
	m.one("labview_node_disk_free_bytes", "gauge",
		"The free space under the machines directory.", float64(free.DiskFreeGB)*(1<<30))
	m.one("labview_node_peers", "gauge", "The peers of this node.", float64(s.peers.Len()))

	ctx, cancel := context.WithTimeout(r.Context(), listingTimeout)
	defer cancel()
	machines := s.inventory.Current().Machines()
	states := s.brokers.States()
	leases := s.leases.All()
	m.one("labview_node_machines", "gauge", "The machines in the inventory of this node.", float64(len(machines)))

	m.family("labview_machine_live", "gauge", "1 when the machine runs.")
	held := make(map[string]bool, len(machines))
	for _, mach := range machines {
		sum := s.summarise(ctx, mach, states, leases)
		held[mach.ID] = sum.Lease != nil
		m.sample("labview_machine_live", flag(sum.Live), "machine", mach.ID)
	}
	m.family("labview_machine_lease_held", "gauge", "1 when a user holds the write lease of the machine.")
	for _, mach := range machines {
		m.sample("labview_machine_lease_held", flag(held[mach.ID]), "machine", mach.ID)
	}

	m.one("labview_lease_grants_total", "counter", "The write leases that this node gave.",
		float64(s.activity.Count(activity.ActionGranted)))
	m.one("labview_lease_denials_total", "counter",
		"The requests that this node refused because the user did not hold the write lease.",
		float64(s.activity.Count(activity.ActionDenied)))
	m.one("labview_peer_failures_total", "counter", "The machine list calls to a peer that failed.",
		float64(s.peerFailCount.Load()))

	w.Header().Set("Content-Type", metricsContentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(m.buf.Bytes())
}
