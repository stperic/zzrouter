package server

import (
	"io"
	"net/http"
	"strconv"

	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// writeNormalizedBody buffers a 2xx body, applies the optional
// per-provider body normalizer, injects the zzrouter routing-metadata
// block (cost, timing, node, provider), and writes the result. Drops
// Transfer-Encoding since the body is being re-encoded.
//
// Caller responsibilities BEFORE calling:
//   - Populate w.Header() with upstream + provider-identifying headers.
//   - Invoke captureNonStreamingMetrics(resp, recorder) if metrics are
//     wanted. This helper consumes resp.Body — once it returns, the
//     metrics-capture window is closed.
//
// After return, the response is fully written.
func writeNormalizedBody(
	w http.ResponseWriter,
	resp *http.Response,
	normalizers *ResponseNormalizers,
	provider string,
	meta wire.RoutingMetadata,
	recorder *llm.InferenceRecorder,
) {
	bodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		w.WriteHeader(resp.StatusCode)
		return
	}
	if bodyNorm := normalizers.Body(provider); bodyNorm != nil {
		if rewritten := bodyNorm(bodyBytes); rewritten != nil {
			bodyBytes = rewritten
		}
	}
	bodyBytes = wire.MaybeInjectRoutingMetadata(bodyBytes, meta, recorder)
	w.Header().Del("Transfer-Encoding")
	w.Header().Set("Content-Length", strconv.Itoa(len(bodyBytes)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(bodyBytes)
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
) {
	w.WriteHeader(resp.StatusCode)
	result := wire.CopyStream(req.Context(), w, resp.Body, recorder,
		wire.NewStreamTransform(normalizers.Stream(provider), meta, recorder).Func())
	endBrokenStream(w, req, result)
}
