package server

import (
	"net/http"

	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

func normalizedBody(normalizers *ResponseNormalizers, provider string, meta wire.RoutingMetadata, recorder *llm.InferenceRecorder) func([]byte) []byte {
	return func(body []byte) []byte {
		if normalize := normalizers.Body(provider); normalize != nil {
			if rewritten := normalize(body); rewritten != nil {
				body = rewritten
			}
		}
		return wire.MaybeInjectRoutingMetadata(body, meta, recorder)
	}
}

// streamWithUsageInject writes a streaming 2xx response through the shared
// stream transform: provider normalizer, usage injection, model restore, strip.
// Stage order and its invariants live in wire.NewStreamTransform.
func streamWithUsageInject(
	req *http.Request,
	w http.ResponseWriter,
	resp *http.Response,
	normalizers *ResponseNormalizers,
	provider string,
	meta wire.RoutingMetadata,
	recorder *llm.InferenceRecorder,
) wire.Result {
	w.WriteHeader(resp.StatusCode)
	result := wire.CopyStream(req.Context(), w, resp.Body, recorder,
		wire.NewStreamTransform(normalizers.Stream(provider), meta, recorder).Func())
	endBrokenStream(w, req, result)
	return result
}
