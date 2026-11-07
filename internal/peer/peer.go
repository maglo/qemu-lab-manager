// Package peer talks to other labview nodes so one wall can show a
// cluster (docs/design/cluster.md section 5).
//
// A peer is another labview. The aggregator speaks the section 5 API.
// No agent and no second protocol. A labview with no peers is unchanged.
package peer

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// namePattern matches a peer name. A name becomes the left side of a
// composite machine id, so it uses the same shape as an inventory id.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ClusterTokenHeader is sent on node-to-node calls when a cluster token
// is configured.
const ClusterTokenHeader = "X-Labview-Cluster-Token"

// Peer is one remote labview node.
type Peer struct {
	Name string
	URL  string // base URL, no trailing slash
}

// ParseFlag reads one -peer value of the form name=url.
func ParseFlag(v string) (Peer, error) {
	name, rawURL, ok := strings.Cut(v, "=")
	if !ok || name == "" || rawURL == "" {
		return Peer{}, fmt.Errorf("peer must be name=url, got %q", v)
	}
	if err := validateName(name); err != nil {
		return Peer{}, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Peer{}, fmt.Errorf("peer %s: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Peer{}, fmt.Errorf("peer %s: URL scheme must be http or https", name)
	}
	if u.Host == "" {
		return Peer{}, fmt.Errorf("peer %s: URL has no host", name)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return Peer{Name: name, URL: u.String()}, nil
}

func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("peer name %q must match %s", name, namePattern.String())
	}
	return nil
}

// JoinID builds the composite id the aggregator serves for a peer machine.
//
// The form is peer:local-id. A path segment cannot hold a slash, and the
// browser already encodes the id, so a colon is the separator.
func JoinID(peerName, localID string) string {
	return peerName + ":" + localID
}

// SplitID reads a composite id. ok is false when id is a local machine id.
func SplitID(id string) (peerName, localID string, ok bool) {
	peerName, localID, found := strings.Cut(id, ":")
	if !found || peerName == "" || localID == "" {
		return "", "", false
	}
	if strings.Contains(localID, ":") {
		return "", "", false
	}
	return peerName, localID, true
}
