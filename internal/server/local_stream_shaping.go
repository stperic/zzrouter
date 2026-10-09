// package server provides HTTP handlers for the zzrouter host server.
// Shared shaping for streams a LOCAL instance serves, so the two paths
// that serve them cannot drift apart.

package server

import (
	"net/http"

	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// localRoutingMetadata describes how a local instance's answer should be
// shaped on the way out.
//
// Local providers have no marginal cost — surfaced via the cost-truth
// contract's "zzrouter, 0" pair rather than by omitting the cost block,
// which would be ambiguous with "we forgot to record".
func (s *Server) localRoutingMetadata(req *http.Request, inst *instance.Instance, servingNode, clientModel string) wire.RoutingMetadata {
	return wire.RoutingMetadata{
		Provider:             inst.Provider,
		Node:                 servingNode,
		InjectUsage:          s.config.Coordinator.Routing.InjectUsageMetadata,
		CostSource:           wire.CostSourceZZRouter,
		CostUSD:              0,
		ClientModel:          clientModel,
		SuppressBodyMetadata: wire.SuppressBodyFromVerbose(req.URL.Query().Get("verbose")),
		SuppressUsageFrame:   suppressUsageFrameFromContext(req.Context()),
	}
}
