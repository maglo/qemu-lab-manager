package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/maglo/qemu-lab-manager/labview/internal/peer"
)

// checkClusterToken refuses a request that carries a wrong cluster token.
//
// A request from a browser carries no token, and it passes. The token says
// that a labview node sent the request, so a wrong one is a node of another
// cluster or a bad configuration (docs/design/cluster.md section 5).
func (s *Server) checkClusterToken(w http.ResponseWriter, r *http.Request) bool {
	got := r.Header.Get(peer.ClusterTokenHeader)
	if got == "" || s.clusterTokenIs(got) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "the cluster token is wrong")
	return false
}

func (s *Server) clusterTokenIs(got string) bool {
	want := s.cfg.ClusterToken
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handleRegisterNode is a node that joins, or renews its place.
func (s *Server) handleRegisterNode(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ClusterToken == "" || s.peers == nil {
		writeError(w, http.StatusForbidden, "this node has no cluster token, so no node can join")
		return
	}
	if !s.clusterTokenIs(r.Header.Get(peer.ClusterTokenHeader)) {
		writeError(w, http.StatusUnauthorized, "a node needs the cluster token to join")
		return
	}
	var reg peer.Registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&reg); err != nil {
		writeError(w, http.StatusBadRequest, "the registration is not valid JSON: %s", err)
		return
	}
	isNew, err := s.peers.Register(reg)
	if errors.Is(err, peer.ErrFlagPeer) {
		writeError(w, http.StatusConflict, "%s", err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	if isNew {
		s.log.Info("node joined", "name", reg.Name, "kvm", reg.KVM)
		if s.telemetry != nil {
			s.telemetry.NodeJoin(reg.Name, reg.KVM)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       reg.Name,
		"ttlSeconds": int(s.peers.TTL().Seconds()),
	})
}

func (s *Server) handleNodes(w http.ResponseWriter, _ *http.Request) {
	nodes := s.peers.Nodes()
	if nodes == nil {
		nodes = []peer.Node{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}
