// package server provides HTTP handlers for the zzrouter host server.
// Shared shaping for streams a LOCAL instance serves, so the two paths
// that serve them cannot drift apart.

package server

import (
	"net/http"

	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/observability/llm"
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

// localStreamTransform is the chunk chain every stream from a local
// instance goes through: the provider's own normalizer, the zzrouter
// usage block, the caller's model name, and finally the strip.
//
// Two call sites serve local instances — proxyToInstance for a warm one
// and streamFromInstance once a cold load has outrun ModelLoadGrace —
// and they used to build this independently, which is how the second one
// ended up with no metering and no strip at all. Build it here so a
// change reaches both.
//
// Strip goes last, and that is only safe because both callers meter from
// the RAW bytes underneath: proxyToInstance wraps the body in
// wire.NewMeteringReader before the normalizing reader, and
// streamFromInstance hands the recorder to CopyWithMetrics, which
// observes each chunk before transforming it. Usage is forced on so a
// request can be metered at all, so without the strip a caller who never
// asked for usage receives an extra frame carrying no choices.
func (s *Server) localStreamTransform(meta wire.RoutingMetadata, provider string, recorder *llm.InferenceRecorder) func([]byte) []byte {
	var inject func([]byte) []byte
	if meta.InjectUsage {
		inject = func(chunk []byte) []byte {
			return wire.InjectUsageIntoSSEChunk(chunk, wire.MetadataWithSnapshot(meta, recorder))
		}
	}

	var restore func([]byte) []byte
	if meta.ClientModel != "" {
		restore = func(chunk []byte) []byte {
			return wire.RewriteModelInStreamChunk(chunk, meta.ClientModel)
		}
	}

	var strip func([]byte) []byte
	if meta.SuppressUsageFrame {
		strip = wire.StripUsageOnlyFrames
	}

	return composeStreamTransforms(
		s.inference.normalizers.Stream(provider),
		normalizer.ChainChunks(inject, restore, strip),
	)
}
