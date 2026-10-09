// Success-path response shaping shared by ProxyClient.ForwardToBackend
// and Server.proxyToRemoteNode. Both sites buffer + optionally
// normalize a 2xx body, inject the zzrouter routing-metadata block
// (cost + timing + node + provider), and write the result with
// correct framing. The streaming path mirrors that pattern with
// per-chunk SSE-usage injection.
//
// proxyToOllamaProvider in ollama_helpers.go does NOT use these —
// the Ollama-direct path doesn't inject routing metadata (the cost-
// truth contract bypasses local-only providers) and falls back to
// streamResponseWithFlush for streaming. Keep them separate.
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
	w http.ResponseWriter,
	resp *http.Response,
	normalizers *ResponseNormalizers,
	provider string,
	meta wire.RoutingMetadata,
	recorder *llm.InferenceRecorder,
) {
	w.WriteHeader(resp.StatusCode)
	copyStreamWithMetrics(w, resp.Body, recorder,
		wire.NewStreamTransform(normalizers.Stream(provider), meta, recorder).Func())
}
