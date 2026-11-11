package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/maglo/qemu-lab-manager/labview/internal/lifecycle"
	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
)

// localNode is the name of this node in a create document, for a node
// that has no -node-name.
const localNode = "local"

// createRequest is the create document, and the node that the client
// names. The node is not part of lifecycle.Spec, because a scenario
// shares that document (docs/design/scenarios.md).
type createRequest struct {
	lifecycle.Spec
	Node string `json:"node,omitempty"`
}

func (s *Server) localName() string {
	if s.cfg.NodeName != "" {
		return s.cfg.NodeName
	}
	return localNode
}

func (s *Server) localCapacity() peer.Capacity {
	if s.capacity != nil {
		return s.capacity()
	}
	dir := ""
	if s.life != nil && s.life.Enabled() {
		dir = s.cfg.MachinesDir
	}
	return peer.LocalCapacity(dir)
}

func (s *Server) handleCapacity(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.localCapacity())
}

// need is what the machine of a create document takes from a node. A
// blank disk is sparse, and it still counts in full: a lab that fills
// its disks must not find out when a guest writes.
func need(spec lifecycle.Spec) peer.Need {
	n := peer.Need{MemoryMB: spec.MemoryMB, VCPUs: spec.VCPUs}
	disks, _ := spec.ResolvedDisks()
	for _, d := range disks {
		if d.Image == "" {
			n.DiskGB += d.SizeGB
		}
	}
	return n
}

// candidate is one node that can be the place of a new machine.
type candidate struct {
	name  string
	peer  *peer.Peer // nil for this node
	free  peer.Capacity
	short string
}

// place finds the node for a create request. target is nil for this
// node. problem is not empty when no node can take the machine.
func (s *Server) place(ctx context.Context, req createRequest) (target *peer.Peer, status int, problem string) {
	if req.Node != "" {
		if req.Node == s.localName() || req.Node == localNode {
			return nil, 0, ""
		}
		p, ok := s.peers.Lookup(req.Node)
		if !ok {
			return nil, http.StatusBadRequest, "no node \"" + req.Node + "\" in the cluster"
		}
		return &p, 0, ""
	}

	cands := s.candidates(ctx)
	n := need(req.Spec)
	var fit []candidate
	var reasons []string
	for _, c := range cands {
		if c.short == "" {
			c.short = c.free.Short(n)
		}
		if c.short == "" {
			fit = append(fit, c)
			continue
		}
		reasons = append(reasons, c.name+": "+c.short)
	}
	if len(fit) == 0 {
		return nil, http.StatusConflict, "no node fits the machine; " + strings.Join(reasons, "; ")
	}
	// The node with the most free memory takes the machine. This node
	// wins a tie, and then the name decides, so the choice is stable.
	sort.SliceStable(fit, func(i, j int) bool {
		if fit[i].free.MemoryFreeMB != fit[j].free.MemoryFreeMB {
			return fit[i].free.MemoryFreeMB > fit[j].free.MemoryFreeMB
		}
		if (fit[i].peer == nil) != (fit[j].peer == nil) {
			return fit[i].peer == nil
		}
		return fit[i].name < fit[j].name
	})
	return fit[0].peer, 0, ""
}

// candidates is this node and each ready peer, with the capacity of each.
func (s *Server) candidates(ctx context.Context) []candidate {
	out := []candidate{{name: s.localName(), free: s.localCapacity()}}

	var remote []candidate
	for _, n := range s.peers.Nodes() {
		p, ok := s.peers.Lookup(n.Name)
		if !ok {
			continue
		}
		c := candidate{name: n.Name, peer: &p}
		if !n.Ready {
			c.short = "no KVM"
		}
		remote = append(remote, c)
	}
	var wg sync.WaitGroup
	for i := range remote {
		if remote[i].short != "" {
			continue
		}
		wg.Add(1)
		go func(c *candidate) {
			defer wg.Done()
			free, err := s.peers.Capacity(ctx, *c.peer)
			if err != nil {
				c.short = "the node did not report its capacity: " + err.Error()
				return
			}
			c.free = free
		}(&remote[i])
	}
	wg.Wait()
	return append(out, remote...)
}

// createOnPeer forwards a create document to the node that place picked.
func (s *Server) createOnPeer(w http.ResponseWriter, r *http.Request, p peer.Peer, spec lifecycle.Spec) {
	doc, err := json.Marshal(spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	status, body, err := s.peers.Create(r.Context(), p, s.identity(r), doc)
	if err != nil {
		writeError(w, http.StatusBadGateway, "node %q did not answer: %s", p.Name, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
