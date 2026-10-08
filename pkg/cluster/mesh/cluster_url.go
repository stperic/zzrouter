package mesh

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// DeriveClusterURL computes the mTLS cluster-port URL for a peer
// endpoint whose public URL is already known. It swaps the scheme to
// https, the port to clusterPort, and preserves host + path.
//
// The coordinator uses its own BindPort as clusterPort by convention:
// deployments are assumed homogeneous on the cluster port. If a future
// heterogeneous-cluster feature lands (workers advertising their own
// cluster port via the pairing-request SANs), this helper becomes a
// fallback for endpoints that haven't advertised yet.
//
// Returns an empty string and an error when publicURL cannot be parsed
// or clusterPort is non-positive. Callers that treat the derived URL as
// optional should surface the empty result without propagating the
// error — Endpoint.ClusterURL being empty is the "not available"
// signal the dispatch path looks for.
func DeriveClusterURL(publicURL string, clusterPort int) (string, error) {
	if clusterPort <= 0 {
		return "", fmt.Errorf("cluster port must be positive: %d", clusterPort)
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return "", fmt.Errorf("parse public URL %q: %w", publicURL, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("public URL %q has no host", publicURL)
	}
	host := u.Hostname()
	out := &url.URL{
		Scheme: "https",
		Host:   net.JoinHostPort(host, strconv.Itoa(clusterPort)),
		Path:   u.Path,
	}
	return out.String(), nil
}
