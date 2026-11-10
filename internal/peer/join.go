package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// NodesPath is where a node registers and where a client lists the nodes.
const NodesPath = "/api/cluster/nodes"

// DefaultJoinInterval is how often a node renews its registration.
const DefaultJoinInterval = 30 * time.Second

// Registration is the body a node sends to the API node.
type Registration struct {
	Name string `json:"name"`
	// URL is where the API node reaches this node.
	URL string `json:"url"`
	KVM bool   `json:"kvm"`
}

// kvmPaths each show that this host has KVM. The module directories are
// there because the labview unit has a private /dev with no /dev/kvm.
var kvmPaths = []string{"/dev/kvm", "/sys/module/kvm_intel", "/sys/module/kvm_amd"}

// KVMPresent reports if this host can run a machine with KVM.
func KVMPresent() bool {
	for _, p := range kvmPaths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Joiner registers this node with the API node, and renews it.
type Joiner struct {
	// APIURL is the base URL of the API node.
	APIURL   string
	Token    string
	Self     Registration
	Interval time.Duration
	Client   *http.Client
	Log      *slog.Logger
}

// Run registers until ctx ends. A failure is logged, and the next
// interval tries again: the API node can start after this node.
func (j *Joiner) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = DefaultJoinInterval
	}
	log := j.Log
	if log == nil {
		log = slog.Default()
	}
	joined := false
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		err := j.Once(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("cluster join failed", "api", j.APIURL, "error", err)
			joined = false
		case err == nil && !joined:
			log.Info("joined the cluster", "api", j.APIURL, "name", j.Self.Name, "kvm", j.Self.KVM)
			joined = true
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once sends one registration.
func (j *Joiner) Once(ctx context.Context) error {
	body, err := json.Marshal(j.Self)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.APIURL+NodesPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ClusterTokenHeader, j.Token)
	client := j.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("status %d: %s", res.StatusCode, truncate(string(bytes.TrimSpace(msg)), 200))
	}
	return nil
}
