// Package discovery exposes hardware + network host probes used by
// the /discover/* HTTP surface and a few startup paths.
//
// Provider/converter tool detection used to live here too, sourced
// from a hardcoded list at pkg/discovery/tools/definitions/. That
// list drifted from the canonical providers/<kind>/<name>/config.yaml
// tree.
// The tools subsystem was retired alongside the corresponding /discover
// routes; agents that need provider listings use /runs/capabilities,
// which reads the providers/ tree directly. The surviving binary-detect
// helpers are at pkg/prov_apps/detect/ — they were never coupled to
// the tools registry.
package discovery

import (
	"context"

	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/discovery/network"
)

// Service provides hardware + network discovery.
type Service struct {
	Hardware *hardware.Discovery
	Network  *network.NodeDiscovery
}

// NewService creates a new discovery service.
func NewService() *Service {
	return &Service{
		Hardware: hardware.NewDiscovery(),
		// No node name, no cluster port — discovery-only service doesn't
		// register on mDNS, just probes.
		Network: network.NewNodeDiscovery("", false, 0),
	}
}

// DiscoverAll performs hardware + network host discovery.
func (s *Service) DiscoverAll(ctx context.Context) (*UnifiedResult, error) {
	result := &UnifiedResult{}

	hosts, err := s.Network.DiscoverNodesWithContext(ctx)
	if err != nil {
		result.Errors = append(result.Errors, err)
	}
	result.NetworkNodes = hosts

	hwInfo, hwErrors := s.Hardware.DiscoverAll(ctx)
	result.Hardware = &hwInfo
	result.Errors = append(result.Errors, hwErrors...)

	return result, nil
}

// UnifiedResult contains discovery results.
type UnifiedResult struct {
	NetworkNodes []*network.NodeEntry   `json:"network_hosts"`
	Hardware     *hardware.HardwareInfo `json:"hardware,omitempty"`
	Errors       []error                `json:"errors,omitempty"`
}
