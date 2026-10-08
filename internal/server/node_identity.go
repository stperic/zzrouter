package server

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// NodeIdentity encapsulates the immutable identity of this server node:
// its name, bind address, port, cluster endpoints, and cluster role.
// All "who am I?" and "do I know this node?" queries go through here.
type NodeIdentity struct {
	name      string
	bind      string
	port      int
	endpoints []string
	mode      pkgConfig.ClusterMode
}

// NewNodeIdentity constructs a NodeIdentity from the server configuration.
func NewNodeIdentity(cfg *pkgConfig.NodeConfig) *NodeIdentity {
	return &NodeIdentity{
		name:      cfg.Node.Name,
		bind:      cfg.Node.Bind,
		port:      cfg.Node.Port,
		endpoints: cfg.Cluster.Endpoints.Addresses(),
		mode:      cfg.Cluster.Mode,
	}
}

// --- Accessors ---

// Name returns the configured node name.
func (n *NodeIdentity) Name() string { return n.name }

// Bind returns the configured bind address.
func (n *NodeIdentity) Bind() string { return n.bind }

// Port returns the configured listen port.
func (n *NodeIdentity) Port() int { return n.port }

// Endpoints returns the configured cluster endpoints.
func (n *NodeIdentity) Endpoints() []string { return n.endpoints }

// Mode returns the cluster mode (coordinator, worker, disabled).
func (n *NodeIdentity) Mode() pkgConfig.ClusterMode { return n.mode }

// --- Hostname resolution ---

// Nodename returns the effective hostname for this server.
//
// It is the same value as Name: the system-hostname fallback lives in
// NodeConfig.applyLoadDefaults, so that every reader of Node.Name gets
// the resolved value, not just the ones that came through here.
func (n *NodeIdentity) Nodename() string {
	if n.name == "" {
		slog.Error("ERROR: No hostname configured and system hostname unavailable")
	}
	return n.name
}

// --- Cluster role queries ---

// IsWorker returns true if this node is a cluster worker.
func (n *NodeIdentity) IsWorker() bool {
	return n.mode == pkgConfig.ClusterModeWorker
}

// IsCoordinator returns true if this node is the cluster coordinator.
func (n *NodeIdentity) IsCoordinator() bool {
	return n.mode == pkgConfig.ClusterModeCoordinator
}

// --- Node matching ---

// IsLocalNode returns true if host refers to this node (empty, "localhost", or matching name).
func (n *NodeIdentity) IsLocalNode(host string) bool {
	return host == "" || host == constants.Localhost || host == n.name
}

// IsKnownNode reports whether node refers to a node this coordinator knows
// about: the local node (by name or bind address) or any configured endpoint.
// The empty string and "*" are treated as "all nodes" and always return true.
func (n *NodeIdentity) IsKnownNode(node string) bool {
	if node == "" || node == "*" {
		return true
	}
	if node == n.name {
		return true
	}
	if node == fmt.Sprintf("%s:%d", n.bind, n.port) {
		return true
	}
	for _, ep := range n.endpoints {
		if ep == node {
			return true
		}
		if idx := strings.Index(ep, ":"); idx > 0 && ep[:idx] == node {
			return true
		}
	}
	return false
}

// KnownNodes returns the list of valid node identifiers, suitable for
// inclusion in a 404 response so clients can retry with a valid node.
func (n *NodeIdentity) KnownNodes() []string {
	nodes := []string{n.name}
	nodes = append(nodes, n.endpoints...)
	return nodes
}

// --- Interface satisfaction ---
// NodeIdentity satisfies NodeInfoProvider (GetNodename, IsWorker)
// and NodeValidator (IsKnownNode, KnownNodes).
// GetLocalStats is NOT on NodeIdentity because it depends on the model registry.

// GetNodename implements NodeInfoProvider.
func (n *NodeIdentity) GetNodename() string {
	return n.Nodename()
}

// MeshAwareNodeValidator wraps a NodeIdentity and consults a runtime
// endpoint source (the coordinator's mesh registry) on every check.
// IsKnownNode succeeds if the static NodeIdentity recognizes the input
// OR any registered peer's name / nodename / alias / URL matches.
//
// Why two layers: NodeIdentity is built from node.yaml at boot; paired
// workers (like "worker-1" or "windows-worker") only become known
// after pairing. Without this wrapper, /runs?node=worker-1 404'd even
// though the cluster routing layer accepted the same name.
//
// EndpointsFn is invoked on every call; cheap (a copy of the registry's
// in-memory snapshot list), no caching needed. Returns nil on workers
// (registry is empty there) and falls through to NodeIdentity-only
// validation, preserving the worker's standalone behavior.
type MeshAwareNodeValidator struct {
	*NodeIdentity
	EndpointsFn func() []*mesh.Endpoint
}

// IsKnownNode returns true if either the static identity or the
// runtime registry recognizes the input.
//
// Registry URLs are stored with a scheme (`http://192.0.2.10:9090`); a
// user-typed `?node=` may use any of three forms — full URL, bare
// `host:port`, or bare `host`. All three normalize to the same peer.
func (v *MeshAwareNodeValidator) IsKnownNode(node string) bool {
	if v.NodeIdentity.IsKnownNode(node) {
		return true
	}
	if v.EndpointsFn == nil {
		return false
	}
	for _, ep := range v.EndpointsFn() {
		if ep == nil {
			continue
		}
		for _, candidate := range endpointIdentifiers(ep) {
			if node == candidate {
				return true
			}
		}
	}
	return false
}

// KnownNodes returns the union of NodeIdentity's static list and every
// non-empty identifier each runtime peer is reachable by (Name, NodeName,
// Alias, URL, host:port, host). Diagnostic surface for 404 bodies — a
// user who hit the wrong spelling sees every accepted form.
func (v *MeshAwareNodeValidator) KnownNodes() []string {
	out := v.NodeIdentity.KnownNodes()
	if v.EndpointsFn == nil {
		return out
	}
	seen := make(map[string]bool, len(out))
	for _, n := range out {
		seen[n] = true
	}
	for _, ep := range v.EndpointsFn() {
		if ep == nil {
			continue
		}
		for _, candidate := range endpointIdentifiers(ep) {
			if candidate == "" || seen[candidate] {
				continue
			}
			out = append(out, candidate)
			seen[candidate] = true
		}
	}
	return out
}

// endpointIdentifiers returns every non-empty form a peer is addressable
// by: the user-visible Name/NodeName/Alias, the full registry URL, and
// the host+port + bare host parsed out of that URL. Used by both
// IsKnownNode (matching) and KnownNodes (diagnostic surface) so the
// two stay in lockstep.
func endpointIdentifiers(ep *mesh.Endpoint) []string {
	out := make([]string, 0, 6)
	for _, s := range []string{ep.Name, ep.NodeName, ep.Alias, ep.URL} {
		if s != "" {
			out = append(out, s)
		}
	}
	if ep.URL == "" {
		return out
	}
	u, err := url.Parse(ep.URL)
	if err != nil || u.Host == "" {
		return out
	}
	if u.Host != "" {
		out = append(out, u.Host)
	}
	if h := u.Hostname(); h != "" && h != u.Host {
		out = append(out, h)
	}
	return out
}
