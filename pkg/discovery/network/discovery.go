package network

import (
	"context"
	"net"
	"os"
	"sync"

	"github.com/grandcat/zeroconf"
	"github.com/stperic/zzrouter/pkg/constants"
)

// cachedInterfaces holds multicast interfaces captured at package init time,
// before gopsutil's netlink usage can corrupt the Go runtime's net.Interfaces().
var cachedInterfaces []net.Interface

// R13 waiver: init() runs before any gopsutil call so we can snapshot
// multicast interfaces while net.Interfaces() is still reliable. See
// docs/package_architecture.md Rule 13 — this is the load-bearing
// workaround exception, not general package state.
func init() {
	ifaces, err := net.Interfaces()
	if err != nil {
		return
	}
	for _, ifi := range ifaces {
		if (ifi.Flags&net.FlagUp) != 0 && (ifi.Flags&net.FlagMulticast) != 0 {
			cachedInterfaces = append(cachedInterfaces, ifi)
		}
	}
}

// CachedMulticastInterfaces returns multicast interfaces captured at init time.
func CachedMulticastInterfaces() []net.Interface {
	return cachedInterfaces
}

// Constants for mDNS discovery
const (
	// Service constants
	ServiceType   = "_zzrouter._tcp"
	ServiceDomain = "local."

	// Port limits
	MinPort = 1
	MaxPort = 65535

	// Nodename validation

	// Channel buffer size for mDNS entries (increased for larger networks)
	ChannelBufferSize = 50
)

// DefaultTimeout is the default timeout for mDNS discovery
var DefaultTimeout = constants.HTTPShortTimeout

// NodeDiscovery handles mDNS-based host discovery for zzRouter
type NodeDiscovery struct {
	server        *zeroconf.Server
	avahiProcess  *os.Process // avahi-publish-service fallback process
	entries       map[string]*NodeEntry
	mu            sync.RWMutex
	isCoordinator bool
	nodeName      string // Configured node name for mDNS registration
	clusterPort   int    // Coordinator mTLS cluster-port (where pairing lives); advertised alongside the public Port.

	// Background discovery fields
	backgroundCtx    context.Context
	backgroundCancel context.CancelFunc
	backgroundDone   chan struct{}
	isRunning        bool
}

// NewNodeDiscovery creates a new host discovery instance.
//
// nodeName is the configured node name (used for mDNS registration;
// falls back to OS hostname when empty). isCoordinator gates mDNS
// registration — only coordinators broadcast themselves, workers
// browse-only. clusterPort is advertised in the coordinator's TXT
// records so browse-only workers can construct the pairing URL
// without pre-configured values; pass 0 on workers and for
// discovery-only consumers (they never register).
//
// The coordinator CA fingerprint is intentionally NOT broadcast:
// mDNS is an unauthenticated LAN channel and "pinning" against a
// broadcast-delivered fingerprint is security theater. Operators
// who want real pinning supply the fingerprint via an OOB channel
// (--ca-fingerprint flag or the --secure wizard prompt).
func NewNodeDiscovery(nodeName string, isCoordinator bool, clusterPort int) *NodeDiscovery {
	return &NodeDiscovery{
		entries:        make(map[string]*NodeEntry),
		nodeName:       nodeName,
		isCoordinator:  isCoordinator,
		clusterPort:    clusterPort,
		backgroundDone: make(chan struct{}),
	}
}

// GetNodes returns a deep copy of all discovered hosts
func (hd *NodeDiscovery) GetNodes() []*NodeEntry {
	hd.mu.RLock()
	defer hd.mu.RUnlock()

	if len(hd.entries) == 0 {
		return []*NodeEntry{}
	}

	// Create deep copy to prevent race conditions
	hosts := make([]*NodeEntry, 0, len(hd.entries))
	for _, entry := range hd.entries {
		// Deep copy the NodeEntry
		hostCopy := &NodeEntry{
			Name:          entry.Name,
			Node:          entry.Node,
			Port:          entry.Port,
			IsCoordinator: entry.IsCoordinator,
			Version:       entry.Version,
			LastSeen:      entry.LastSeen, // Time is immutable
		}
		hosts = append(hosts, hostCopy)
	}

	return hosts
}
