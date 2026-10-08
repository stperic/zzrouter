package server

import (
	"context"
	"log/slog"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/discovery/network"
)

// clusterResourceGroup pairs the resource tracker and mDNS discovery
// under a single Start/Stop so Server's lifecycle can treat them as one
// subsystem. No behavior of its own — it's a two-field wrapper that
// exists solely to satisfy the Start/Stop pairing guard.
//
// Private to internal/server because there is no cross-package caller
// and the group adds no domain boundary. Originally lived in
// pkg/clusternode; demoted here when the authoritative clusternode
// package (the mTLS cluster HTTP server) landed.
type clusterResourceGroup struct {
	resources *mesh.ResourceTracker
	discovery *network.NodeDiscovery
	port      int
}

func newClusterResourceGroup(resources *mesh.ResourceTracker, discovery *network.NodeDiscovery, port int) *clusterResourceGroup {
	return &clusterResourceGroup{
		resources: resources,
		discovery: discovery,
		port:      port,
	}
}

// Start starts resource collection and mDNS registration. mDNS failure
// is degraded-ok — logged but not returned, so nodes without multicast-
// capable networks still come up cleanly.
func (g *clusterResourceGroup) Start(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.resources.Start(ctx)
	if err := g.discovery.Start(g.port); err != nil {
		slog.Warn("mDNS registration failed (discovery will be unavailable)", "error", err)
	}
	return nil
}

// Stop stops mDNS and resource tracking. Safe to call multiple times —
// both underlying subsystems use sync.Once guards.
func (g *clusterResourceGroup) Stop(_ context.Context) error {
	if g == nil {
		return nil
	}
	g.resources.Stop()
	g.discovery.Stop()
	return nil
}
