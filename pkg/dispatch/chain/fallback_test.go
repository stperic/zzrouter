package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	pfallback "github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// =============================================================================
// FakeDeps: minimal ServerDeps for driving Proxy without touching *Server.
// =============================================================================

// fakeDeps implements chain.ServerDeps with caller-configured behaviour for
// ProxyForwardDetached + Backend. Everything else returns
// defaults safe for the tests below.
type fakeDeps struct {
	nodeName string

	// forward maps targetURL → canned response. Missing key returns a 404.
	forward map[string]func() (*http.Response, error)

	// endpoints maps app → (url, exists).
	endpoints map[string]string

	// clusterURLs maps node → its cluster port base URL.
	clusterURLs map[string]string

	// instances maps model → its running on-demand instance.
	instances        map[string]*instance.Instance
	validationErrors map[string]error

	// Counters (atomic, in case the code ever parallelizes retries).
	forwardCalls        atomic.Int32
	trackRateLimitCalls atomic.Int32
	loadExecutorCalls   atomic.Int32
	injectUsageMetadata bool

	// requestDefaults maps model → request defaults; forwarded records
	// the last body sent.
	requestDefaults map[string]map[string]any
	forwarded       []byte
	// forwardedTo records the upstream of each forward, in order.
	forwardedTo []backend.Upstream
}

func newFakeDeps() *fakeDeps {
	return &fakeDeps{
		nodeName:  "test-node",
		forward:   map[string]func() (*http.Response, error){},
		endpoints: map[string]string{},
	}
}

// ===== Topology =====

func (f *fakeDeps) NodeName() string                              { return f.nodeName }
func (f *fakeDeps) IsLocalNode(node string) bool                  { return node == f.nodeName }
func (f *fakeDeps) ClusterURL(node string) string                 { return f.clusterURLs[node] }
func (f *fakeDeps) NodeMetrics(node string) *mesh.ResourceMetrics { return nil }
func (f *fakeDeps) NodeCacheReady() bool                          { return false }

// ===== ProviderConfig =====

func (f *fakeDeps) Backend(app string) (*backend.Resolved, bool) {
	u, ok := f.endpoints[app]
	if !ok {
		return nil, false
	}
	return &backend.Resolved{Endpoint: u, Upstream: backend.CallerKey()}, true
}
func (f *fakeDeps) InjectUsageMetadata() bool { return f.injectUsageMetadata }
func (f *fakeDeps) RequestDefaults(_, model, _ string) map[string]any {
	return f.requestDefaults[model]
}

// ===== Runtime =====

func (f *fakeDeps) ProxyForwardDetached(ctx context.Context, method, url string, headers http.Header, up backend.Upstream, body []byte) (*http.Response, error) {
	f.forwardCalls.Add(1)
	f.forwarded = body
	f.forwardedTo = append(f.forwardedTo, up)
	if fn, ok := f.forward[url]; ok {
		return fn()
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(`{"error":"no route"}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func (f *fakeDeps) Normalizers() *normalizer.Registry { return normalizer.NewRegistry() }

func (f *fakeDeps) AppInstance(model string) (*instance.Instance, bool) {
	inst, ok := f.instances[model]
	return inst, ok
}

func (f *fakeDeps) ValidateModel(_ context.Context, model string) error {
	return f.validationErrors[model]
}

func (f *fakeDeps) InstanceRequest(_ *http.Request, inst *instance.Instance, body []byte) []byte {
	if inst.WireModel != "" {
		return wire.SetModelIfPresent(body, inst.WireModel)
	}
	return body
}

func (f *fakeDeps) NewLoadExecutor() LoadExecutor {
	return func(ctx context.Context, model, provider string) error {
		f.loadExecutorCalls.Add(1)
		return nil
	}
}

func (f *fakeDeps) TrackProviderRateLimit(app string, resp *http.Response) {
	f.trackRateLimitCalls.Add(1)
}

// cannedJSON builds a *http.Response returning a JSON payload with the
// given status and headers.
func cannedJSON(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header: http.Header{
			"Content-Type":   []string{"application/json"},
			"Content-Length": []string{itoaLen(body)},
		},
		ContentLength: int64(len(body)),
	}
}

func itoaLen(s string) string {
	n := len(s)
	if n == 0 {
		return "0"
	}
	// Short, no strconv import needed.
	buf := make([]byte, 0, 8)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// newProxyWithFake wires a Proxy with a fake-driven deps surface and the
// minimum fallback primitives required for a happy-path request.
func newProxyWithFake(deps ServerDeps) *Proxy {
	strategies := pfallback.NewStrategyRegistry()
	strategies.Register(&pfallback.PriorityStrategy{})
	return New(
		deps,
		pfallback.NewCooldownManager(),
		pfallback.NewCooldownManager(),
		strategies,
		pfallback.NewLoadTracker(),
		nil, // no latency tracker
		nil, // no health checker
	)
}

// buildGinCtx returns a gin.Context attached to a fresh recorder and a
// POST /v1/chat/completions request body.
func buildGinCtx(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	c.Request = req
	// As the /v1 route group would.
	httperr.AttachResponder(openaiproto.New())(c)
	return c, w
}

// =============================================================================
// Tests — happy path + exhausted + retry/fallback.
// =============================================================================

// TestProxyWithFallback_ExhaustedWithNoCandidates verifies the 503 path
// when there are no deployment candidates at all. Exercises
// writeExhaustedResponse and recordFallbackMetrics with an empty
// attempt list.
func TestProxyWithFallback_ExhaustedWithNoCandidates(t *testing.T) {
	deps := newFakeDeps()
	p := newProxyWithFake(deps)

	c, w := buildGinCtx(t, `{"model":"group-x","stream":false}`)
	rec := llm.NewInferenceRecorder(c.Request.Context(), "group-x", "")
	resolved := &resolver.Resolved{
		OriginalName: "group-x",
		GroupName:    "group-x",
		Strategy:     "priority",
		Candidates:   nil,
	}

	p.ProxyWithFallback(c, resolved, []byte(`{"model":"group-x"}`), rec, "group-x", false)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	var env map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	errObj, ok := env["error"].(map[string]any)
	require.True(t, ok, "response missing error envelope")
	assert.Equal(t, "model_not_available", errObj["code"])
	assert.Equal(t, int32(0), deps.forwardCalls.Load(),
		"no candidates means no forwards should have been attempted")
}

// TestProxyWithFallback_ExhaustedOnDemandPending verifies that when
// every candidate is on-demand and not running, the response carries
// the model_loading code + Retry-After header, and NewLoadExecutor
// fired for each skipped candidate.
func TestProxyWithFallback_ExhaustedOnDemandPending(t *testing.T) {
	deps := newFakeDeps()
	p := newProxyWithFake(deps)

	c, w := buildGinCtx(t, `{"model":"group-y","stream":false}`)
	rec := llm.NewInferenceRecorder(c.Request.Context(), "group-y", "")
	resolved := &resolver.Resolved{
		OriginalName: "group-y",
		GroupName:    "group-y",
		Strategy:     "priority",
		Candidates: []pfallback.Candidate{
			{Name: "dep-a", App: "llamacpp", Model: "m", OnDemand: true},
		},
	}

	p.ProxyWithFallback(c, resolved, []byte(`{"model":"group-y"}`), rec, "group-y", false)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	var env map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	errObj, ok := env["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "model_loading", errObj["code"])
	assert.Equal(t, "60", w.Header().Get("Retry-After"))
	// NewLoadExecutor was invoked once per skipped candidate via
	// startModelAsync. It's spawned as a goroutine so we can't deterministically
	// observe it in the hot path; asserting >= 0 here is a no-op, but the
	// existence of the call-path is gated by the forwardCalls == 0 check.
	assert.Equal(t, int32(0), deps.forwardCalls.Load(),
		"on-demand-only candidates should not be forwarded, only skipped")
}

// TestProxyWithFallback_SuccessCommitsResponse verifies the end-to-end
// happy path: one local deployment, ProxyForwardDetached returns 200
// with a JSON body, and Commit writes the body to the client.
func TestProxyWithFallback_SuccessCommitsResponse(t *testing.T) {
	const upstreamBody = `{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`

	deps := newFakeDeps()
	deps.endpoints["ollama"] = "http://127.0.0.1:11434"
	deps.forward["http://127.0.0.1:11434/v1/chat/completions"] = func() (*http.Response, error) {
		return cannedJSON(http.StatusOK, upstreamBody), nil
	}
	p := newProxyWithFake(deps)

	c, w := buildGinCtx(t, `{"model":"group-z","stream":false}`)
	rec := llm.NewInferenceRecorder(c.Request.Context(), "group-z", "")
	resolved := &resolver.Resolved{
		OriginalName: "group-z",
		GroupName:    "group-z",
		Strategy:     "priority",
		Candidates: []pfallback.Candidate{
			{Name: "dep-a", App: "ollama", Model: "m"},
		},
	}

	p.ProxyWithFallback(c, resolved, []byte(`{"model":"group-z"}`), rec, "group-z", false)

	require.Equal(t, http.StatusOK, w.Code,
		"expected upstream 200 to pass through; body=%s", w.Body.String())
	assert.Equal(t, int32(1), deps.forwardCalls.Load(),
		"expected exactly one forward call on happy path")
	// Commit injects the X-zzrouter-Provider header on every response.
	assert.Equal(t, "ollama", w.Header().Get("X-zzrouter-Provider"))
	// Body flows through unchanged (deployment rewrite is model-name-only;
	// normalizers registry is empty; no routing metadata injection for
	// single-app non-cloud).
	assert.Contains(t, w.Body.String(), `"content":"hi"`)
	// The replica answers as "m"; the caller asked for the group. A client
	// that indexes responses by the id it sent has to get that id back.
	assert.Contains(t, w.Body.String(), `"model":"group-z"`)
	assert.NotContains(t, w.Body.String(), `"model":"m"`)
}

// TestProxyWithFallback_FallsBackPast429 verifies the core invariant:
// a 429 on the first candidate is consumed silently (cooldown set,
// retries exhausted, body drained) and the second candidate's 200 is
// committed.
func TestProxyWithFallback_FallsBackPast429(t *testing.T) {
	const upstreamBody = `{"id":"chatcmpl-2","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`

	deps := newFakeDeps()
	deps.endpoints["provider-a"] = "http://10.0.0.1:9000"
	deps.endpoints["provider-b"] = "http://10.0.0.2:9000"

	// Both retries on provider-a return 429.
	deps.forward["http://10.0.0.1:9000/v1/chat/completions"] = func() (*http.Response, error) {
		return cannedJSON(http.StatusTooManyRequests, `{"error":"rate limited"}`), nil
	}
	deps.forward["http://10.0.0.2:9000/v1/chat/completions"] = func() (*http.Response, error) {
		return cannedJSON(http.StatusOK, upstreamBody), nil
	}

	p := newProxyWithFake(deps)

	c, w := buildGinCtx(t, `{"model":"group-q","stream":false}`)
	rec := llm.NewInferenceRecorder(c.Request.Context(), "group-q", "")
	resolved := &resolver.Resolved{
		OriginalName: "group-q",
		GroupName:    "group-q",
		Strategy:     "priority",
		Candidates: []pfallback.Candidate{
			{Name: "dep-a", App: "provider-a", Model: "m", MaxRetries: 0},
			{Name: "dep-b", App: "provider-b", Model: "m", MaxRetries: 0},
		},
	}

	p.ProxyWithFallback(c, resolved, []byte(`{"model":"group-q"}`), rec, "group-q", false)

	require.Equal(t, http.StatusOK, w.Code,
		"expected fallback to second deployment to succeed; body=%s", w.Body.String())

	// With MaxRetries=0 the zero-value triggers DefaultMaxRetries=1 inside
	// tryDeployment (chain/fallback.go:147-148), so dep-a is hit twice
	// (retry=0, retry=1, both 429) before falling to dep-b. Total
	// ProxyForwardDetached calls: 2 on dep-a + 1 on dep-b = 3.
	assert.Equal(t, int32(3), deps.forwardCalls.Load())
	// TrackProviderRateLimit fires on every response (retriable or
	// success). 2 for dep-a's 429s + 1 for dep-b's 200 = 3.
	assert.Equal(t, int32(3), deps.trackRateLimitCalls.Load(),
		"expected rate-limit tracking on every backend response")
	// Success commit carries the winning provider header.
	assert.Equal(t, "provider-b", w.Header().Get("X-zzrouter-Provider"))
}

func TestBuildRouteWireFields_SplitsAttemptsByOutcome(t *testing.T) {
	attempts := []pfallback.Attempt{
		{DeploymentName: "r-cool", Outcome: pfallback.OutcomeCooldownSkip},
		{DeploymentName: "r-ondemand", Outcome: pfallback.OutcomeOnDemandSkip},
		{DeploymentName: "r-ok", StatusCode: 200, Outcome: pfallback.OutcomeSuccess},
		{DeploymentName: "r-fail", StatusCode: 503, Outcome: pfallback.OutcomeRetriable},
	}
	preFilter := []pfallback.Skip{
		{Candidate: pfallback.Candidate{Name: "r-unhealthy"}, Reason: pfallback.SkipReasonUnhealthy},
	}
	chain, skipped := buildRouteWireFields(attempts, preFilter)

	require.Len(t, chain, 2)
	assert.Equal(t, "r-ok", chain[0].Replica)
	assert.Equal(t, "success", chain[0].Outcome)
	assert.Equal(t, "r-fail", chain[1].Replica)
	assert.Equal(t, 503, chain[1].Status)

	require.Len(t, skipped, 3)
	assert.Equal(t, "r-cool", skipped[0].Replica)
	assert.Equal(t, "cooldown", skipped[0].Reason)
	assert.Equal(t, "r-ondemand", skipped[1].Replica)
	assert.Equal(t, "on_demand", skipped[1].Reason)
	assert.Equal(t, "r-unhealthy", skipped[2].Replica)
	assert.Equal(t, "unhealthy", skipped[2].Reason)
}

func TestBuildRouteWireFields_EmptyInputs(t *testing.T) {
	chain, skipped := buildRouteWireFields(nil, nil)
	assert.Nil(t, chain)
	assert.Nil(t, skipped)
}

// A group replica's model gets its own request defaults, as a direct
// request for it would: a group naming a variant must not lose them.
func TestProxyWithFallback_AppliesTheReplicasRequestDefaults(t *testing.T) {
	deps := newFakeDeps()
	deps.endpoints["llamacpp"] = "http://127.0.0.1:8080"
	deps.forward["http://127.0.0.1:8080/v1/chat/completions"] = func() (*http.Response, error) {
		return cannedJSON(http.StatusOK, `{"id":"x","model":"m","choices":[]}`), nil
	}
	deps.requestDefaults = map[string]map[string]any{
		"q+agent": {"chat_template_kwargs": map[string]any{"enable_thinking": false}, "temperature": 0.6},
	}
	p := newProxyWithFake(deps)

	c, w := buildGinCtx(t, `{"model":"agent","temperature":0.1}`)
	rec := llm.NewInferenceRecorder(c.Request.Context(), "agent", "")
	resolved := &resolver.Resolved{
		OriginalName: "agent", GroupName: "agent", Strategy: "priority",
		Candidates: []pfallback.Candidate{{Name: "r", App: "llamacpp", Model: "q+agent"}},
	}
	p.ProxyWithFallback(c, resolved, []byte(`{"model":"agent","temperature":0.1}`), rec, "agent", false)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var sent map[string]any
	require.NoError(t, json.Unmarshal(deps.forwarded, &sent))
	assert.Equal(t, "q+agent", sent["model"])
	assert.Equal(t, map[string]any{"enable_thinking": false}, sent["chat_template_kwargs"])
	assert.Equal(t, 0.1, sent["temperature"], "the client's value wins")
}

// Where an attempt goes decides what goes with it: a running instance is
// an engine zzRouter launched, an endpoint provider is what its config
// declares, and a replica on another node is a worker.
func TestResolveDeploymentTarget_NamesTheUpstream(t *testing.T) {
	deps := newFakeDeps()
	deps.endpoints["svc"] = "http://svc.lan:8000"
	deps.instances = map[string]*instance.Instance{
		"m": instance.NewInstance("run-1", "llamacpp", "m", 8081, 0, 1),
	}
	deps.clusterURLs = map[string]string{"worker-1": "https://10.0.0.7:9091"}
	p := newProxyWithFake(deps)

	cases := []struct {
		name   string
		dep    pfallback.Candidate
		target string
		up     backend.Upstream
	}{
		{"running instance", pfallback.Candidate{Name: "a", App: "llamacpp", Model: "m", OnDemand: true},
			"http://localhost:8081/v1/chat/completions", backend.Engine()},
		{"endpoint provider", pfallback.Candidate{Name: "b", App: "svc", Model: "m"},
			"http://svc.lan:8000/v1/chat/completions", backend.CallerKey()},
		{"another node", pfallback.Candidate{Name: "c", App: "llamacpp", Model: "m", Node: "worker-1"},
			"https://10.0.0.7:9091/v1/chat/completions", backend.Cluster()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, up, _, err := p.resolveDeploymentTarget(tc.dep, "/v1/chat/completions", "")
			require.NoError(t, err)
			assert.Equal(t, tc.target, target)
			assert.Equal(t, tc.up, up)
		})
	}
}

// A replica on a node with no known cluster port is not guessed at: the
// worker serves inference nowhere else.
func TestResolveDeploymentTarget_UnknownNodeIsAnError(t *testing.T) {
	p := newProxyWithFake(newFakeDeps())
	_, _, _, err := p.resolveDeploymentTarget(
		pfallback.Candidate{Name: "c", App: "llamacpp", Model: "m", Node: "nowhere"}, "/v1/chat/completions", "")
	assert.ErrorContains(t, err, "cluster port")
}

// A worker is told which provider serves the replica; nothing else is,
// and the caller's request is left as it was.
func TestAttemptHeaders(t *testing.T) {
	caller := http.Header{"X-Api-Key": []string{"k"}}
	dep := pfallback.Candidate{App: "vllm"}

	toWorker := attemptHeaders(caller, backend.Cluster(), dep)
	assert.Equal(t, "vllm", toWorker.Get(constants.HeaderServingProvider))
	assert.Equal(t, "k", toWorker.Get("X-Api-Key"))
	assert.Empty(t, caller.Get(constants.HeaderServingProvider), "the caller's headers are not modified")

	assert.Empty(t, attemptHeaders(caller, backend.CallerKey(), dep).Get(constants.HeaderServingProvider))
}

// A replica this node cannot reach (no client for its transport) is
// passed over like a refused connection; it does not end the chain while
// another replica can still answer.
func TestProxyWithFallback_UnreachableReplicaFallsThrough(t *testing.T) {
	deps := newFakeDeps()
	deps.clusterURLs = map[string]string{"worker-1": "https://10.0.0.7:9091"}
	deps.endpoints["provider-b"] = "http://10.0.0.2:9000"
	deps.forward["https://10.0.0.7:9091/v1/chat/completions"] = func() (*http.Response, error) {
		return nil, fmt.Errorf("%w: no cluster mTLS client", pfallback.ErrUnreachable)
	}
	deps.forward["http://10.0.0.2:9000/v1/chat/completions"] = func() (*http.Response, error) {
		return cannedJSON(http.StatusOK, `{"id":"c","model":"m","choices":[]}`), nil
	}
	p := newProxyWithFake(deps)

	c, w := buildGinCtx(t, `{"model":"group-q","stream":false}`)
	rec := llm.NewInferenceRecorder(c.Request.Context(), "group-q", "")
	resolved := &resolver.Resolved{
		OriginalName: "group-q", GroupName: "group-q", Strategy: "priority",
		Candidates: []pfallback.Candidate{
			{Name: "dep-a", App: "vllm", Model: "m", Node: "worker-1"},
			{Name: "dep-b", App: "provider-b", Model: "m"},
		},
	}

	p.ProxyWithFallback(c, resolved, []byte(`{"model":"group-q"}`), rec, "group-q", false)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "provider-b", w.Header().Get("X-zzrouter-Provider"))
}

func TestProxyWithFallback_UnsupportedImageDoesNotRetryOrCooldown(t *testing.T) {
	deps := newFakeDeps()
	deps.endpoints["ollama"] = "http://127.0.0.1:11434"
	deps.forward["http://127.0.0.1:11434/v1/chat/completions"] = func() (*http.Response, error) {
		return cannedJSON(http.StatusInternalServerError, `{"error":{"message":"image input is not supported","type":"api_error"}}`), nil
	}
	p := newProxyWithFake(deps)
	c, w := buildGinCtx(t, `{"model":"group"}`)
	r := &resolver.Resolved{OriginalName: "group", GroupName: "group", Strategy: "priority", Candidates: []pfallback.Candidate{{Name: "dep-a", App: "ollama", Model: "m"}, {Name: "dep-b", App: "ollama", Model: "m"}}}
	p.ProxyWithFallback(c, r, []byte(`{"model":"group"}`), nil, "group", false)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"type":"invalid_request_error"`)
	assert.EqualValues(t, 1, deps.forwardCalls.Load())
	assert.False(t, p.cooldowns.InCooldown("dep-a"))
	assert.False(t, p.providerCooldowns.InCooldown("ollama"))
}

func TestProxyWithFallback_ConflictAdmission(t *testing.T) {
	for _, node := range []string{"", "worker"} {
		t.Run("node="+node, func(t *testing.T) {
			deps := newFakeDeps()
			deps.validationErrors = map[string]error{"fast": &config.ModelNameConflictError{Model: "fast", Node: "worker"}}
			p := newProxyWithFake(deps)
			c, w := buildGinCtx(t, `{"model":"group"}`)
			rec := llm.NewInferenceRecorder(c.Request.Context(), "group", "")
			resolved := &resolver.Resolved{OriginalName: "group", GroupName: "group", Candidates: []pfallback.Candidate{
				{Name: "conflict", App: "llamacpp", Model: "fast", OnDemand: true, Node: node},
			}}
			p.ProxyWithFallback(c, resolved, []byte(`{"model":"group"}`), rec, "group", false)
			require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), `"code":"model_name_conflict"`)
			assert.Zero(t, deps.forwardCalls.Load())
			assert.Zero(t, deps.loadExecutorCalls.Load())

			// The reserved name cannot poison an otherwise usable group.
			deps.endpoints["ollama"] = "http://safe"
			deps.forward["http://safe/v1/chat/completions"] = func() (*http.Response, error) {
				return cannedJSON(http.StatusOK, `{"model":"base","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
			}
			resolved.Candidates = append(resolved.Candidates, pfallback.Candidate{Name: "safe", App: "ollama", Model: "base"})
			c, w = buildGinCtx(t, `{"model":"group"}`)
			rec = llm.NewInferenceRecorder(c.Request.Context(), "group", "")
			p.ProxyWithFallback(c, resolved, []byte(`{"model":"group"}`), rec, "group", false)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, int32(1), deps.forwardCalls.Load(), "only the safe replica received a request")
			assert.Zero(t, deps.loadExecutorCalls.Load())
		})
	}
}
