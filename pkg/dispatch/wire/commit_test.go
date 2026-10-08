package wire

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/utils"
)

// buildJSONResp constructs a synthetic upstream response carrying a JSON
// body with Content-Type + matching Content-Length. No Transfer-Encoding.
func buildJSONResp(status int, body string, extraHeaders map[string]string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	for k, v := range extraHeaders {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode:    status,
		Body:          io.NopCloser(strings.NewReader(body)),
		Header:        h,
		ContentLength: int64(len(body)),
	}
}

// buildSSEResp constructs a synthetic upstream response with SSE framing.
func buildSSEResp(body string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	return &http.Response{
		StatusCode:    http.StatusOK,
		Body:          io.NopCloser(strings.NewReader(body)),
		Header:        h,
		ContentLength: -1,
	}
}

// TestCommit_NonStreamingInjectsProviderOnInferenceResponse verifies
// the non-streaming happy path: usage-shaped inference responses pick
// up the root zzrouter block (provider only when no deployment/cost
// is known); Content-Length matches the rewritten body length; the
// X-zzrouter-Provider header is set.
func TestCommit_NonStreamingInjectsProviderOnInferenceResponse(t *testing.T) {
	body := `{"id":"chatcmpl-1","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "ollama")
	norms := normalizer.NewRegistry()

	Commit(w, resp, "ollama", rec, norms)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ollama", w.Header().Get("X-zzrouter-Provider"))

	out := w.Body.String()
	assert.Contains(t, out, `"zzrouter":`)
	assert.Contains(t, out, `"provider":"ollama"`)
	cl, err := strconv.Atoi(w.Header().Get("Content-Length"))
	require.NoError(t, err)
	assert.Equal(t, len(out), cl,
		"Content-Length header must match rewritten body length")
}

// TestCommitWithRouting_InjectsZzrouterBlockAndHeaders verifies that
// when a deployment+node are provided for a non-streaming response, the
// zzrouter metadata block is injected into the body and the matching
// response headers are set.
func TestCommitWithRouting_InjectsZzrouterBlockAndHeaders(t *testing.T) {
	body := `{"id":"chatcmpl-2","choices":[{"message":{"content":"ok"}}]}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
	norms := normalizer.NewRegistry()

	CommitWithRouting(w, resp, RoutingMetadata{Provider: "openai", Deployment: "dep-a", Node: "node-1"}, rec, norms)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "openai", w.Header().Get("X-zzrouter-Provider"))
	assert.Equal(t, "dep-a", w.Header().Get("X-zzrouter-Deployment"))
	assert.Equal(t, "node-1", w.Header().Get("X-zzrouter-Node"))

	// Body now carries a "zzrouter" root-level block.
	out := w.Body.String()
	assert.Contains(t, out, `"zzrouter":`)
	assert.Contains(t, out, `"provider":"openai"`)
	assert.Contains(t, out, `"deployment":"dep-a"`)
	assert.Contains(t, out, `"node":"node-1"`)

	// Content-Length is set to the rewritten length.
	cl, err := strconv.Atoi(w.Header().Get("Content-Length"))
	require.NoError(t, err)
	assert.Equal(t, len(out), cl,
		"Content-Length header must match rewritten body length")
}

// TestCommitWithRouting_NoTransferEncodingOnRewrittenBody enforces the
// RFC 7230 invariant: an outgoing response must not carry both
// Content-Length and Transfer-Encoding. After the non-streaming body
// rewrite, Transfer-Encoding must be absent from the client-facing
// response.
//
// Note: the `maps.Copy` + `IsStreaming` ordering means that a chunked
// upstream is always classified as streaming (IsStreaming reads TE and
// returns true), so the non-streaming code path never actually sees a
// chunked TE in practice. This test exercises the scrub line defensively
// — it asserts that the post-Commit response carries NO TE header
// regardless of what upstream emitted, so future refactors that break
// the streaming/non-streaming split can't silently re-introduce the
// dual-encoding bug.
func TestCommitWithRouting_NoTransferEncodingOnRewrittenBody(t *testing.T) {
	body := `{"id":"chatcmpl-3","choices":[]}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
	norms := normalizer.NewRegistry()

	Commit(w, resp, "openai", rec, norms)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Transfer-Encoding"),
		"Transfer-Encoding must be absent when Content-Length is set")
	assert.NotEmpty(t, w.Header().Get("Content-Length"))
}

// TestCommitWithRouting_BodyNormalizerRuns registers a MLX-like
// reasoning→content shim and verifies the body is rewritten before
// zzrouter injection.
func TestCommitWithRouting_BodyNormalizerRuns(t *testing.T) {
	body := `{"id":"chatcmpl-4","choices":[{"message":{"reasoning":"thinking","content":""}}]}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")

	norms := normalizer.NewRegistry()
	// Stub normalizer replaces empty content with reasoning.
	norms.RegisterBody("mlx", func(b []byte) []byte {
		return []byte(strings.Replace(string(b), `"content":""`, `"content":"thinking"`, 1))
	})

	CommitWithRouting(w, resp, RoutingMetadata{Provider: "mlx", Deployment: "dep-x"}, rec, norms)

	out := w.Body.String()
	assert.Contains(t, out, `"content":"thinking"`,
		"body normalizer output should be visible in committed response")
	assert.NotContains(t, out, `"content":""`,
		"original empty content should have been rewritten")
}

// TestCommitWithRouting_StreamingPreservesFraming verifies the
// streaming branch: upstream SSE body passes through, X-zzrouter-*
// headers are set before WriteHeader (so the client sees them in the
// initial response), and [DONE] is present exactly once.
func TestCommitWithRouting_StreamingPreservesFraming(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	resp := buildSSEResp(upstream)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
	norms := normalizer.NewRegistry()

	CommitWithRouting(w, resp, RoutingMetadata{Provider: "openai", Deployment: "dep-a", Node: "node-1"}, rec, norms)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "openai", w.Header().Get("X-zzrouter-Provider"))
	assert.Equal(t, "dep-a", w.Header().Get("X-zzrouter-Deployment"))
	assert.Equal(t, "node-1", w.Header().Get("X-zzrouter-Node"))

	out := w.Body.String()
	// SSE payload flows through unchanged (injectUsageMetadata=false so
	// no chunk transform runs).
	assert.Equal(t, 1, strings.Count(out, "data: [DONE]"),
		"exactly one [DONE] frame must appear on the wire")
	assert.Contains(t, out, `"content":"hi"`)
}

// TestCommitWithRouting_SyntheticUsageChunkOnMissingUsage verifies that
// when injectUsageMetadata=true and the upstream emits no usage
// object, Commit writes a synthetic final chunk carrying zz_* fields
// so routing metadata reaches clients.
func TestCommitWithRouting_SyntheticUsageChunkOnMissingUsage(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	resp := buildSSEResp(upstream)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
	norms := normalizer.NewRegistry()

	CommitWithRouting(w, resp, RoutingMetadata{Provider: "openai", Deployment: "dep-a", Node: "node-1", InjectUsage: true}, rec, norms)

	out := w.Body.String()
	// Synthetic chunk carries zz_* fields.
	assert.Contains(t, out, `"zz_provider":"openai"`)
	assert.Contains(t, out, `"zz_model":"dep-a"`)
	assert.Contains(t, out, `"zz_node":"node-1"`)
}

// TestCommitWithRouting_TokensPerSecReachesBody confirms the
// recorder's tokensOut stash + Snapshot's TokensPerSec computation
// flow into the response body when the noise floor is cleared.
// CaptureNonStreamingMetrics → recordProxyMetrics → RecordCompletion
// stashes tokensOut on the recorder, then the inject reads via
// withSnapshot.
func TestCommitWithRouting_TokensPerSecReachesBody(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	restore := utils.SetClock(utils.FixedClock(now))
	defer restore()
	body := `{"id":"x","choices":[{"message":{"content":"hi"}}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":50,"cost":0.0001}}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openrouter")
	norms := normalizer.NewRegistry()

	_ = utils.SetClock(utils.FixedClock(now.Add(time.Second)))
	CommitWithRouting(w, resp, RoutingMetadata{
		Provider: "openrouter", Deployment: "openai/gpt-4", Node: "node-1",
	}, rec, norms)

	out := w.Body.String()
	assert.Contains(t, out, `"tokens_per_second":`,
		"tokens_per_second should reach the response body when output_tokens >= noise floor")
	// The synthetic one-second request also supplies deterministic latency.
	assert.Contains(t, out, `"latency_ms":1000`)
}

// TestCommitWithRouting_TokensPerSecOmittedBelowFloor confirms the
// noise floor: a 1-token completion produces no tokens_per_second.
func TestCommitWithRouting_TokensPerSecOmittedBelowFloor(t *testing.T) {
	body := `{"id":"x","choices":[{"message":{"content":"."}}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":1,"cost":0.0001}}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openrouter")
	norms := normalizer.NewRegistry()

	CommitWithRouting(w, resp, RoutingMetadata{
		Provider: "openrouter", Deployment: "openai/gpt-4",
	}, rec, norms)

	out := w.Body.String()
	assert.NotContains(t, out, `"tokens_per_second"`,
		"completion_tokens=1 is below the noise floor; field must be omitted")
}

// TestCommitWithRouting_OpenRouterCostFlowsToBody is the end-to-end
// non-streaming path for the cost-truth fix: an OpenRouter-shaped
// upstream body with usage.cost reaches CommitWithRouting; the parser
// tags CostSource=provider; the recorder snapshot feeds the inject;
// the response body carries cost_usd + cost_source: "provider".
func TestCommitWithRouting_OpenRouterCostFlowsToBody(t *testing.T) {
	body := `{"id":"gen-1","choices":[{"message":{"content":"hi"}}],` +
		`"usage":{"prompt_tokens":19,"completion_tokens":3,"cost":0.000001989}}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openrouter")
	norms := normalizer.NewRegistry()

	CommitWithRouting(w, resp, RoutingMetadata{
		Provider:    "openrouter",
		Deployment:  "openai/gpt-4",
		Node:        "node-1",
		InjectUsage: false,
	}, rec, norms)

	out := w.Body.String()
	assert.Contains(t, out, `"cost_source":"provider"`,
		"upstream usage.cost ⇒ cost_source=provider must reach the response body")
	assert.Contains(t, out, `"cost_usd":0.000001989`,
		"upstream cost number must reach the response body")
}

// TestCommitWithRouting_UsageChunkInjectedWhenPresent verifies that
// when the upstream DOES emit a usage object, injectUsageMetadata=true
// rewrites it to carry zz_* fields instead of emitting a synthetic
// chunk afterward.
func TestCommitWithRouting_UsageChunkInjectedWhenPresent(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	resp := buildSSEResp(upstream)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
	norms := normalizer.NewRegistry()

	CommitWithRouting(w, resp, RoutingMetadata{Provider: "openai", Deployment: "dep-a", Node: "node-1", InjectUsage: true}, rec, norms)

	out := w.Body.String()
	// zz_* fields injected INTO the usage chunk (not as a separate
	// synthetic chunk).
	assert.Contains(t, out, `"zz_provider":"openai"`)
	assert.Contains(t, out, `"prompt_tokens":5`)
	// No synthetic chat.completion.chunk object shape — the real chunk
	// is the one that got rewritten.
	assert.Equal(t, 1, strings.Count(out, `"zz_provider":"openai"`),
		"usage injection should happen exactly once, not twice")
}

func TestCommitWithRouting_RouteAgentHeaders(t *testing.T) {
	body := `{"id":"chatcmpl-3","usage":{"prompt_tokens":5,"completion_tokens":7}}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
	norms := normalizer.NewRegistry()

	CommitWithRouting(w, resp, RoutingMetadata{
		Provider:     "openai",
		Deployment:   "r-primary",
		GroupName:    "fast-chat",
		StrategyUsed: "priority",
		FallbackChain: []FallbackAttempt{
			{Replica: "r-primary", Status: 200, Outcome: "success"},
		},
	}, rec, norms)

	assert.Equal(t, "r-primary", w.Header().Get("X-zzrouter-Replica"),
		"Replica header carries the selected replica name (agent vocabulary)")
	assert.Equal(t, "r-primary", w.Header().Get("X-zzrouter-Deployment"),
		"Deployment header is preserved during the rename window")
	assert.Equal(t, "fast-chat", w.Header().Get("X-zzrouter-Route"))
	assert.Equal(t, "priority", w.Header().Get("X-zzrouter-Strategy"))
	assert.Equal(t, "1", w.Header().Get("X-zzrouter-Fallback-Count"))
}

func TestCommitWithRouting_OmitsRouteHeadersWhenEmpty(t *testing.T) {
	body := `{"id":"x"}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
	norms := normalizer.NewRegistry()

	CommitWithRouting(w, resp, RoutingMetadata{Provider: "openai"}, rec, norms)

	for _, h := range []string{"X-zzrouter-Route", "X-zzrouter-Strategy", "X-zzrouter-Fallback-Count", "X-zzrouter-Replica"} {
		assert.Empty(t, w.Header().Get(h), "%s must be omitted when its source value is empty", h)
	}
}

func TestCommitWithRouting_FallbackCountReflectsAttemptCount(t *testing.T) {
	body := `{"id":"x"}`
	resp := buildJSONResp(http.StatusOK, body, nil)
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
	norms := normalizer.NewRegistry()

	CommitWithRouting(w, resp, RoutingMetadata{
		Provider:   "openai",
		Deployment: "r3",
		FallbackChain: []FallbackAttempt{
			{Replica: "r1", Status: 503, Outcome: "retriable"},
			{Replica: "r2", Status: 503, Outcome: "retriable"},
			{Replica: "r3", Status: 200, Outcome: "success"},
		},
	}, rec, norms)
	assert.Equal(t, "3", w.Header().Get("X-zzrouter-Fallback-Count"))
}

// A model-group stream answered in the replica's model name because the
// restore only ran on the non-streaming path. The id has to come back in
// the caller's spelling on every frame, and it must not depend on
// InjectUsage — the frames that carry no usage still name a model.
func TestCommitWithRouting_StreamingRestoresClientModel(t *testing.T) {
	const upstream = "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"

	for _, injectUsage := range []bool{false, true} {
		t.Run("inject_usage="+strconv.FormatBool(injectUsage), func(t *testing.T) {
			resp := buildSSEResp(upstream)
			defer func() { _ = resp.Body.Close() }()
			w := httptest.NewRecorder()
			rec := llm.NewInferenceRecorder(context.Background(), "route-x", "ollama")

			CommitWithRouting(w, resp, RoutingMetadata{
				Provider:    "ollama",
				Deployment:  "ollama-node-a",
				GroupName:   "route-x",
				ClientModel: "route-x",
				InjectUsage: injectUsage,
			}, rec, normalizer.NewRegistry())

			body := w.Body.String()
			assert.NotContains(t, body, `"model":"m"`, "replica name leaked to the client")
			assert.Equal(t, 2, strings.Count(body, `"model":"route-x"`),
				"every frame carrying a model must carry the caller's id; body=%s", body)
		})
	}
}

// An empty ClientModel is the "nothing to restore" case: paths that never
// learned the caller's id must pass the engine's answer through untouched.
func TestCommitWithRouting_StreamingLeavesModelAloneWhenUnknown(t *testing.T) {
	resp := buildSSEResp("data: {\"model\":\"m\",\"choices\":[]}\n\n")
	defer func() { _ = resp.Body.Close() }()
	w := httptest.NewRecorder()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "ollama")

	CommitWithRouting(w, resp, RoutingMetadata{Provider: "ollama"}, rec, normalizer.NewRegistry())

	assert.Contains(t, w.Body.String(), `"model":"m"`)
}
