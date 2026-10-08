package wire

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// Commit writes a previously-detached backend response to the client.
// Used after proxy.ForwardDetached confirms a successful status code.
func Commit(w http.ResponseWriter, resp *http.Response, providerType string, recorder *llm.InferenceRecorder, norms *normalizer.Registry) {
	CommitWithRouting(w, resp, RoutingMetadata{Provider: providerType}, recorder, norms)
}

// CommitWithRouting writes the response plus routing metadata headers
// and optional in-body usage injection. Streaming and non-streaming
// responses both flow through this function.
//
// Ordering invariant — capture metrics → inject response → fire
// RecordCompletion hook → bridge settles ledger. The response inject
// runs synchronously inside this function before the hook fires, so
// the ledger settle path observes the same recorder state the
// response did.
//
// Other invariants preserved from pre-extraction
// (commitDetachedResponseWithRouting):
//   - Headers (including X-zzrouter-*) are set BEFORE WriteHeader.
//   - Non-streaming path re-sets Content-Length to match rewritten bytes.
//   - Streaming path may emit a synthetic final usage chunk when
//     InjectUsage is true and the upstream did not include one;
//     note that CopyWithMetrics may synthesize a [DONE] sentinel before
//     returning, so the synthetic usage chunk can currently follow
//     [DONE] in the happy path — pre-existing ordering behavior is
//     preserved verbatim.
func CommitWithRouting(w http.ResponseWriter, resp *http.Response, meta RoutingMetadata, recorder *llm.InferenceRecorder, norms *normalizer.Registry) {
	streaming := IsStreaming(resp)

	if !streaming {
		CaptureNonStreamingMetrics(resp, recorder)
	}

	// Both branches rewrite the body, so the upstream's framing is dropped:
	// a streamed rewrite under the engine's Content-Length is cut off.
	MergeUpstreamHeaders(w.Header(), resp.Header)
	w.Header().Set(constants.HeaderServingProvider, meta.Provider)
	if meta.Deployment != "" {
		// X-zzrouter-Replica is the agent-vocabulary header name for the
		// selected replica; X-zzrouter-Deployment is the same value
		// under the older dispatch-chain term. Both are emitted until
		// the rename sweep retires Deployment.
		w.Header().Set("X-zzrouter-Replica", meta.Deployment)
		w.Header().Set("X-zzrouter-Deployment", meta.Deployment)
	}
	if meta.Node != "" {
		w.Header().Set(constants.HeaderServingNode, meta.Node)
	}
	if meta.GroupName != "" {
		w.Header().Set("X-zzrouter-Route", meta.GroupName)
	}
	if meta.StrategyUsed != "" {
		w.Header().Set("X-zzrouter-Strategy", meta.StrategyUsed)
	}
	if n := len(meta.FallbackChain); n > 0 {
		// The count reflects actual dispatch tries; pre-filter skips
		// surface separately on the body's `skipped` array. An n of 1
		// means "no fallback occurred"; the header is still set so
		// agents can distinguish "trivial dispatch" from "no metadata".
		w.Header().Set("X-zzrouter-Fallback-Count", strconv.Itoa(n))
	}

	if streaming {
		var inner func([]byte) []byte
		usageSeen := false
		if meta.InjectUsage {
			inner = func(chunk []byte) []byte {
				// Test for a real usage OBJECT, not the substring: the
				// copier offers every chunk, so being called proves
				// nothing, and `"usage":null` rides along on every delta
				// frame OpenAI and vLLM emit. A substring test goes true
				// on frame one and the synthetic-metadata fallback below
				// would then never fire on a provider that sends no usage
				// at all -- which is the case it exists for.
				if ChunkHasUsageObject(chunk) {
					usageSeen = true
				}
				// Build per-chunk metadata from the live recorder
				// snapshot so the terminal usage frame can carry cost
				// + timing values that were populated by the parser
				// before this chunk was forwarded.
				return InjectUsageIntoSSEChunk(chunk, MetadataWithSnapshot(meta, recorder))
			}
		}
		// Restoring the caller's model id is not metadata: it rides on
		// every frame, so it must not sit behind the InjectUsage gate
		// above. Without it a model-group stream answered in the
		// replica's model name, which is not the id the caller sent and
		// not one the catalog lists.
		//
		// Frames reach this transform whole: CopyWithMetrics re-aligns
		// its reads to line boundaries, so one split across two TCP
		// segments arrives rejoined rather than as truncated JSON the
		// rewrite would pass through untouched.
		var restore func([]byte) []byte
		if meta.ClientModel != "" {
			restore = func(chunk []byte) []byte {
				return RewriteModelInStreamChunk(chunk, meta.ClientModel)
			}
		}
		// Last in the chain, so usageSeen above and the recorder (which
		// CopyWithMetrics feeds from the raw chunk before any transform)
		// both still observe the frame this drops.
		var strip func([]byte) []byte
		if meta.SuppressUsageFrame {
			strip = StripUsageOnlyFrames
		}
		transform := normalizer.ComposeStream(norms.Stream(meta.Provider),
			normalizer.ChainChunks(inner, restore, strip))
		w.WriteHeader(resp.StatusCode)
		CopyWithMetrics(w, resp.Body, recorder, transform)

		// If the provider didn't include a usage block in any streaming
		// chunk, emit a synthetic final chunk so clients (e.g. Open WebUI)
		// can still display routing metadata via the zz_* fields.
		// Skipped when the caller did not ask for usage: synthesising the
		// very frame the strip above removes would hand it back.
		if meta.InjectUsage && !usageSeen && meta.Deployment != "" && !meta.SuppressUsageFrame {
			usage := map[string]any{
				"zz_provider": meta.Provider,
				"zz_model":    meta.Deployment,
			}
			if meta.Node != "" {
				usage["zz_node"] = meta.Node
			}
			full := MetadataWithSnapshot(meta, recorder)
			applyExtras(usage, full, "zz_")
			synth := map[string]any{
				"object":  "chat.completion.chunk",
				"choices": []any{},
				"usage":   usage,
			}
			if data, err := json.Marshal(synth); err == nil {
				chunk := append([]byte("data: "), data...)
				chunk = append(chunk, []byte("\n\n")...)
				_, _ = w.Write(chunk)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	} else {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err == nil && len(bodyBytes) > 0 {
			if bodyNorm := norms.Body(meta.Provider); bodyNorm != nil {
				if rewritten := bodyNorm(bodyBytes); rewritten != nil {
					bodyBytes = rewritten
				}
			}
			bodyBytes = MaybeInjectRoutingMetadata(bodyBytes, meta, recorder)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(bodyBytes)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(bodyBytes)
	}
}

// MaybeInjectRoutingMetadata applies the canonical gate that decides
// whether a non-streaming response body gets the zzrouter block. The
// signal for "this is an inference response" is the presence of a
// "usage" key at the top level — every OpenAI- and Ollama-shape
// inference response carries one, while non-inference pass-through
// (files, models, etc.) does not. Cost fields are independently
// gated by snap.CostSource via applyExtras: providers that don't
// surface upstream cost (OpenAI direct, Anthropic direct, every local
// provider) still get provider/node/latency on the wire — just no
// cost block, which matches the contract.
//
// meta.SuppressBodyMetadata short-circuits the injection so an agent
// that opted out via ?verbose=0 still gets the response headers but
// none of the in-body metadata.
//
// Returns the (possibly rewritten) body. Caller is responsible for
// Content-Length / Transfer-Encoding rewriting around it. Used by
// both wire.CommitWithRouting (model-group + chain routing) and
// internal/server.ProxyClient.ForwardToBackend (direct cloud + service
// pass-through), so the gate logic stays in one place.
func MaybeInjectRoutingMetadata(body []byte, meta RoutingMetadata, recorder *llm.InferenceRecorder) []byte {
	// Restoring the caller's model id happens before every gate below.
	// It is not metadata: ?verbose=0 opts out of the zzrouter block, not
	// out of getting back an id that matches the catalog, and a response
	// that carries no usage object still names a model. SetModelIfPresent
	// only ever rewrites a model that is already there, so a body without
	// one is returned untouched.
	body = RestoreClientModel(body, meta.ClientModel)

	if meta.SuppressBodyMetadata {
		return body
	}
	// json.Unmarshal accepts leading whitespace per RFC 8259, and some
	// upstream gateways pretty-print or pad their bodies. Find the first
	// non-whitespace byte before deciding this is non-JSON.
	first := -1
	for i, b := range body {
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		first = i
		break
	}
	if first < 0 || body[first] != '{' {
		return body
	}
	if !bytes.Contains(body, []byte(`"usage"`)) && meta.Deployment == "" {
		return body
	}
	snap := MetadataWithSnapshot(meta, recorder)
	return InjectRoutingMetadata(body, snap)
}

// RestoreClientModel rewrites a response body's "model" field to the id
// the caller used. A no-op when the caller's id is unknown, so paths
// that never learned one are unaffected.
func RestoreClientModel(body []byte, clientModel string) []byte {
	if clientModel == "" {
		return body
	}
	return SetModelIfPresent(body, clientModel)
}

// MetadataWithSnapshot returns a RoutingMetadata copy with cost+timing
// fields populated from the recorder's current state. Caller-provided
// fields (Provider/Deployment/Node/InjectUsage) take precedence; cost
// fields are filled only when the caller left them zero. Exported so
// the server-side proxy can build the same metadata for direct-cloud
// pass-through (which doesn't go through CommitWithRouting).
func MetadataWithSnapshot(base RoutingMetadata, recorder *llm.InferenceRecorder) RoutingMetadata {
	if recorder == nil {
		return base
	}
	snap := recorder.Snapshot()
	if base.CostSource == "" && snap.CostSource != "" {
		base.CostSource = CostSource(snap.CostSource)
		base.CostUSD = snap.Cost
	}
	if base.LatencyMs == 0 {
		base.LatencyMs = snap.LatencyMs
	}
	if base.TTFTMs == 0 {
		base.TTFTMs = snap.TTFTMs
	}
	if base.TokensPerSec == 0 {
		base.TokensPerSec = snap.TokensPerSec
	}
	return base
}
