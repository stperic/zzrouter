package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	metricSDK "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/model"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

func budgetKey(t *testing.T, s *Server, keyLimit, teamLimit float64) string {
	t.Helper()
	resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey,
		map[string]any{"id": "budget-team", "name": "Budget", "spend_limit": teamLimit})
	require.Equal(t, http.StatusCreated, resp.Code, string(resp.Body))
	resp = makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey,
		map[string]any{"id": "budget-key", "name": "Budget", "team_id": "budget-team", "spend_limit": keyLimit})
	require.Equal(t, http.StatusCreated, resp.Code, string(resp.Body))
	var out struct {
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &out))
	require.NotEmpty(t, out.Data.Key)
	return out.Data.Key
}

func budgetNode(t *testing.T, order []string, backends map[string]*serviceBackend) *Server {
	t.Helper()
	s := newServiceGroupNode(t, order, backends)
	disabled := false
	s.model.Pricing = pricing.NewStore(&pkgConfig.PricingConfig{Enabled: &disabled}, t.TempDir())
	return s
}

func TestBudgetPricingScopes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key, team float64
		status    int
	}{
		{"key", 1, 0, 403}, {"team", 0, 1, 403}, {"both", 1, 1, 403}, {"neither", 0, 0, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &serviceBackend{status: 200, wire: []string{"chat_completions"}}
			s := budgetNode(t, []string{"unknown"}, map[string]*serviceBackend{"unknown": b})
			raw := budgetKey(t, s, tc.key, tc.team)
			resp := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", raw, map[string]any{"model": "claude", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
			require.Equal(t, tc.status, resp.Code, string(resp.Body))
			if tc.status == 403 {
				assert.Contains(t, string(resp.Body), `"code":"model_unpriced"`)
				assert.Contains(t, string(resp.Body), `"type":"permission_error"`)
				assert.Contains(t, string(resp.Body), "unknown-model")
				assert.Contains(t, string(resp.Body), "/zzrouter/v1/pricing/overrides")
				assert.Empty(t, resp.Headers.Get("Retry-After"))
				assert.NotContains(t, string(resp.Body), "spend_limit")
				assert.Empty(t, b.requests())
				assert.Equal(t, map[string]int64{"unknown/unknown-model": 1}, s.model.Pricing.Misses())
			} else {
				assert.Len(t, b.requests(), 1)
			}
		})
	}
}

func TestPricingExplicitZero(t *testing.T) {
	disabled := false
	store := pricing.NewStore(&pkgConfig.PricingConfig{Enabled: &disabled}, t.TempDir())
	require.NoError(t, store.SetOverride(pricing.Override{Provider: "cloud", Model: "zero"}))
	assert.NotNil(t, pricing.Resolve(store, "cloud", []string{"zero"}, false).Rates)
	s := &Server{model: &model.Subsystem{Pricing: store}, appsConfig: &pkgConfig.AppsConfig{}}
	enabled := true
	for _, tc := range []struct {
		name, mode string
		protocol   pkgConfig.AppProtocol
		runtime    *pkgConfig.AppRuntimeConfig
		free       bool
	}{
		{"launched", "on-demand", pkgConfig.ProtocolOpenAI, &pkgConfig.AppRuntimeConfig{BasePort: 12345, Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "unused"}}, true},
		{"ollama", "service", pkgConfig.ProtocolOllama, &pkgConfig.AppRuntimeConfig{Endpoint: "http://unused.invalid"}, true},
		{"ollama-cloud", "cloud", pkgConfig.ProtocolOllama, &pkgConfig.AppRuntimeConfig{Endpoint: "http://unused.invalid", API: &pkgConfig.APIConfig{AuthType: "bearer", Token: "unused"}}, false},
		{"external", "service", pkgConfig.ProtocolOpenAI, &pkgConfig.AppRuntimeConfig{Endpoint: "http://unused.invalid"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, s.appsConfig.AddApp(tc.name, pkgConfig.ServiceConfig{Name: tc.name, Mode: tc.mode, Protocol: tc.protocol, Enabled: &enabled, Runtime: tc.runtime, Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			target := &resolver.Resolved{Provider: tc.name, ModelName: "model"}
			s.quoteTargets(c, target)
			assert.Equal(t, tc.free, target.Quotes[0].Rates != nil)
			if tc.free {
				require.NotNil(t, target.Quotes[0].Rates)
				assert.Zero(t, target.Quotes[0].Rates.InputCostPerToken)
				require.NoError(t, store.SetOverride(pricing.Override{Provider: tc.name, Model: "model", InputCostPerToken: 0.01}))
				s.quoteTargets(c, target)
				assert.Equal(t, 0.01, target.Quotes[0].Rates.InputCostPerToken)
			}
		})
	}
}

func TestBudgetFreeAfterExhaustion(t *testing.T) {
	rig := newAccessRig(t)
	rig.seedTeam(t, "team", &teams.Team{Name: "Team", QuotaConfig: quota.QuotaConfig{SpendLimit: 1}})
	raw := rig.seedKey(t, "key", "team", string(teams.RoleMember), &keys.VirtualKey{Name: "Key", Role: "user", QuotaConfig: quota.QuotaConfig{SpendLimit: 1}})
	request := func(quote pricing.Quote, cost int64) (bool, int) {
		c, w := rig.makeRequest(raw)
		require.True(t, rig.access.Authenticate(c, RoleUser))
		httperr.AttachResponder(newResponderSet().openai)(c)
		ok := rig.access.Enforce(c, "model", func() ([]pricing.Quote, bool) { return []pricing.Quote{quote}, true })
		if ok {
			rig.access.RecordSpendByKey("key", reservationIDFromCtx(c), cost, 10, 5)
			rig.access.CancelPendingReservationIfUnsettled(c)
			ReleaseConcurrency(c)
		}
		return ok, w.Code
	}
	paid := pricing.Quote{Rates: &pricing.ModelPricing{InputCostPerToken: 1}}
	ok, _ := request(paid, 2_000_000)
	require.True(t, ok)
	ok, status := request(paid, 0)
	require.False(t, ok)
	assert.Equal(t, 402, status)
	ok, status = request(pricing.Quote{Rates: &pricing.ModelPricing{}}, 0)
	require.True(t, ok)
	assert.Equal(t, 200, status)
	for _, state := range []*quota.SpendState{rig.access.GetKeySpend("key"), rig.access.GetTeamSpend("team")} {
		require.NotNil(t, state)
		assert.EqualValues(t, 2, state.RequestCount)
		assert.EqualValues(t, 20, state.TokensIn)
		assert.EqualValues(t, 2_000_000, state.SpendMicro)
		assert.Zero(t, state.ReservedMicro)
	}
}

func TestBudgetUnpricedFallbackChain(t *testing.T) {
	for _, first := range []bool{true, false} {
		t.Run(map[bool]string{true: "unknown-first", false: "unknown-last"}[first], func(t *testing.T) {
			order := []string{"priced", "unknown"}
			if first {
				order = []string{"unknown", "priced"}
			}
			backends := map[string]*serviceBackend{"priced": {status: 200, wire: []string{"chat_completions"}}, "unknown": {status: 200, wire: []string{"chat_completions"}}}
			s := budgetNode(t, order, backends)
			raw := budgetKey(t, s, 1, 1)
			require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "priced", Model: "priced-model", InputCostPerToken: 0.01}))
			resp := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", raw, map[string]any{"model": "claude", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
			require.Equal(t, 200, resp.Code, string(resp.Body))
			assert.Len(t, backends["priced"].requests(), 1)
			assert.Empty(t, backends["unknown"].requests())
		})
	}
}

func TestBudgetPricingSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name, path       string
		wire             []string
		stream, affinity bool
	}{
		{"chat", "/v1/chat/completions", []string{"chat_completions"}, false, false},
		{"chat-stream", "/v1/chat/completions", []string{"chat_completions"}, true, false},
		{"messages", "/v1/messages", []string{"messages"}, false, false},
		{"messages-stream", "/v1/messages", []string{"messages"}, true, false},
		{"messages-translation", "/v1/messages", []string{"chat_completions", "messages_compat"}, false, false},
		{"responses", "/v1/responses", []string{"responses"}, false, false},
		{"responses-affinity", "/v1/responses", []string{"responses"}, false, true},
		{"responses-translation", "/v1/responses", []string{"chat_completions", "responses_compat"}, true, false},
		{"embeddings", "/v1/embeddings", []string{"embeddings"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &serviceBackend{status: 200, wire: tc.wire}
			s := budgetNode(t, []string{"unknown"}, map[string]*serviceBackend{"unknown": b})
			raw := budgetKey(t, s, 0, 1)
			body := map[string]any{"model": "claude", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 4, "input": "hi", "stream": tc.stream}
			if tc.affinity {
				s.inference.affinity.Record("resp_prior", "unknown")
				body["previous_response_id"] = "resp_prior"
			}
			resp := makeAuthRequest(t, s, http.MethodPost, tc.path, raw, body)
			require.Equal(t, 403, resp.Code, string(resp.Body))
			assert.Contains(t, string(resp.Body), "/zzrouter/v1/pricing/overrides")
			assert.Empty(t, b.requests())
			assert.NotContains(t, resp.Headers.Get("Content-Type"), "text/event-stream")
		})
	}
}

func TestBudgetQuoteSurvivesSettlement(t *testing.T) {
	for _, response := range []string{"", "unknown-response"} {
		t.Run(response, func(t *testing.T) {
			rig := newAccessRig(t)
			rig.seedTeam(t, "team", &teams.Team{Name: "Team"})
			raw := rig.seedKey(t, "key", "team", string(teams.RoleMember), &keys.VirtualKey{Name: "Key", Role: "user", QuotaConfig: quota.QuotaConfig{SpendLimit: 1}})
			s := &Server{access: rig.access, model: &model.Subsystem{Pricing: pricing.NewStore(&bridgePricingConfig{}, t.TempDir())}}
			require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "paid", Model: "concrete", InputCostPerToken: 0.001}))
			c, _ := rig.makeRequest(raw)
			require.True(t, rig.access.Authenticate(c, RoleUser))
			recorder := llm.NewInferenceRecorder(c.Request.Context(), "alias", "")
			target, release, ok := s.enforceAndAttribute(c, "alias", recorder, func() (*resolver.Resolved, bool) {
				r := &resolver.Resolved{Provider: "paid", ModelName: "concrete"}
				s.quoteTargets(c, r)
				return r, true
			})
			require.True(t, ok)
			defer release()
			require.Equal(t, "concrete", target.Quotes[0].Model)
			bridge := NewInferenceLogBridge(inferencelog.NewStore(8, 0), "node", false, s.model.Pricing)
			bridge.SetAccessControl(rig.access)
			llm.SetInferenceLogHook(bridge)
			t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
			require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "paid", Model: "alias", InputCostPerToken: 0.1}))
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := s.model.Pricing.DeleteOverride("paid", "concrete")
				assert.NoError(t, err)
			}()
			<-done
			recorder.SetProvider("paid")
			recorder.SetResponseModel(response)
			recorder.RecordCompletion(10, 0)
			state := rig.access.GetKeySpend("key")
			require.NotNil(t, state)
			assert.EqualValues(t, 10_000, state.SpendMicro)
			assert.Zero(t, state.ReservedMicro)
			assert.Empty(t, s.model.Pricing.Misses())
		})
	}
}

func TestBudgetPricingRollbackAndOrder(t *testing.T) {
	rig := newAccessRig(t)
	rig.seedTeam(t, "team", &teams.Team{Name: "Team", AllowedModels: []string{"allowed"}})
	raw := rig.seedKey(t, "key", "team", string(teams.RoleMember), &keys.VirtualKey{Name: "Key", Role: "user", QuotaConfig: quota.QuotaConfig{SpendLimit: 1, MaxParallelRequests: 1}})
	prepare := func() ([]pricing.Quote, bool) { return paidQuotes(), true }
	c, _ := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	called := false
	assert.False(t, rig.access.Enforce(c, "forbidden", func() ([]pricing.Quote, bool) { called = true; return prepare() }))
	assert.False(t, called)
	c, _ = rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	assert.False(t, rig.access.Enforce(c, "allowed", func() ([]pricing.Quote, bool) { return nil, false }))
	assert.Nil(t, rig.access.GetKeySpend("key"))
	first, _ := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(first, RoleUser))
	require.True(t, rig.access.Enforce(first, "allowed", prepare))
	held := rig.access.GetKeySpend("key").ReservedMicro
	second, _ := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(second, RoleUser))
	assert.False(t, rig.access.Enforce(second, "allowed", prepare))
	assert.Equal(t, held, rig.access.GetKeySpend("key").ReservedMicro)
	rig.access.CancelPendingReservationIfUnsettled(first)
	ReleaseConcurrency(first)
	assert.Zero(t, rig.access.GetKeySpend("key").ReservedMicro)
	// A resolution failure precedes quota checks and therefore consumes no RPM.
	c, _ = rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	assert.False(t, rig.access.Enforce(c, "allowed", func() ([]pricing.Quote, bool) { return nil, false }))
	assert.Zero(t, rig.access.GetKeySpend("key").ReservedMicro)
	func() {
		c, _ := rig.makeRequest(raw)
		require.True(t, rig.access.Authenticate(c, RoleUser))
		ctx, cancel := context.WithCancel(c.Request.Context())
		c.Request = c.Request.WithContext(ctx)
		s := &Server{access: rig.access}
		_, release, ok := s.enforceAndAttribute(c, "allowed", nil, func() (*resolver.Resolved, bool) {
			return &resolver.Resolved{Provider: "test", ModelName: "model", Quotes: paidQuotes()}, true
		})
		defer release()
		require.True(t, ok)
		cancel()
	}()
	assert.Zero(t, rig.access.GetKeySpend("key").ReservedMicro)
}

func TestBudgetPricingWorkerAndCountTokens(t *testing.T) {
	b := &serviceBackend{status: 200, wire: []string{"messages"}}
	s := budgetNode(t, []string{"unknown"}, map[string]*serviceBackend{"unknown": b})
	raw := budgetKey(t, s, 1, 1)
	resp := makeAuthRequest(t, s, http.MethodPost, "/v1/messages/count_tokens", raw, map[string]any{"model": "claude", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	require.Equal(t, 200, resp.Code, string(resp.Body))
	assert.Nil(t, s.access.GetKeySpend("budget-key"))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{}`))
	c.Request = c.Request.WithContext(context.Background())
	setAccessContext(c, &control.AccessContext{Key: &control.KeyPrincipal{Role: control.RoleAdmin, Quotas: quota.QuotaConfig{SpendLimit: 1}}})
	called := false
	assert.True(t, s.access.Enforce(c, "model", func() ([]pricing.Quote, bool) { called = true; return nil, true }))
	assert.True(t, called)
}

func TestBudgetPricingAfterWireSelection(t *testing.T) {
	for _, tc := range []struct {
		name, path, native, compat string
	}{
		{"messages", "/v1/messages", "messages", "messages_compat"},
		{"responses", "/v1/responses", "responses", "responses_compat"},
	} {
		for _, supported := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/supported=%t", tc.name, supported), func(t *testing.T) {
				wire := []string{"embeddings"}
				pricedWire := []string{"embeddings"}
				if supported {
					wire = []string{tc.native}
					pricedWire = []string{"chat_completions", tc.compat}
				}
				backends := map[string]*serviceBackend{
					"unknown": {status: 200, wire: wire},
					"priced":  {status: 200, wire: pricedWire},
				}
				s := budgetNode(t, []string{"unknown", "priced"}, backends)
				raw := budgetKey(t, s, 1, 0)
				require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "priced", Model: "priced-model"}))
				resp := makeAuthRequest(t, s, http.MethodPost, tc.path, raw, map[string]any{"model": "claude", "max_tokens": 4, "input": "hi", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
				if supported {
					require.Equal(t, 403, resp.Code, string(resp.Body))
					assert.Contains(t, string(resp.Body), `"code":"model_unpriced"`)
					assert.Equal(t, map[string]int64{"unknown/unknown-model": 1}, s.model.Pricing.Misses())
				} else {
					require.Equal(t, 400, resp.Code, string(resp.Body))
					assert.Contains(t, string(resp.Body), "declares neither")
					assert.Empty(t, s.model.Pricing.Misses(), "wire rejection precedes pricing")
				}
				assert.Empty(t, backends["unknown"].requests())
				assert.Empty(t, backends["priced"].requests())
				assert.Nil(t, s.access.GetKeySpend("budget-key"))
			})
		}
	}
}

func TestBudgetQuoteSurvivesFallback(t *testing.T) {
	backends := map[string]*serviceBackend{
		"busy":  {status: 503, wire: []string{"chat_completions"}},
		"spare": {status: 200, wire: []string{"chat_completions"}, reply: `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`},
	}
	s := budgetNode(t, []string{"busy", "spare"}, backends)
	raw := budgetKey(t, s, 1, 0)
	require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "busy", Model: "busy-model", InputCostPerToken: 0.01}))
	require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "spare", Model: "spare-model", InputCostPerToken: 0.001}))
	bridge := NewInferenceLogBridge(inferencelog.NewStore(8, 0), "node", false, nil)
	bridge.SetAccessControl(s.access)
	llm.SetInferenceLogHook(bridge)
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
	resp := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", raw, map[string]any{"model": "claude", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	require.Equal(t, 200, resp.Code, string(resp.Body))
	assert.NotEmpty(t, backends["busy"].requests())
	assert.Len(t, backends["spare"].requests(), 1)
	state := s.access.GetKeySpend("budget-key")
	require.NotNil(t, state)
	assert.EqualValues(t, 2000, state.SpendMicro)
	assert.Zero(t, state.ReservedMicro)
	entries := bridge.store.Query(inferencelog.QueryFilter{Limit: 8})
	require.Len(t, entries, 1)
	assert.Equal(t, "spare", entries[0].App)
}

func TestBudgetPricingDirectAndOllama(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/api/chat", "/api/generate", "/api/embed"} {
		t.Run(path, func(t *testing.T) {
			b := &serviceBackend{status: 200, wire: []string{"chat_completions"}}
			s := budgetNode(t, []string{"ollama"}, map[string]*serviceBackend{"ollama": b})
			raw := budgetKey(t, s, 1, 0)
			s.model.Resolver = staticModelResolver{provider: "ollama"}
			resp := makeAuthRequest(t, s, http.MethodPost, path, raw, map[string]any{"model": "ollama-model", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "prompt": "hi", "input": "hi"})
			require.Equal(t, 403, resp.Code, string(resp.Body))
			assert.Contains(t, string(resp.Body), "/zzrouter/v1/pricing/overrides")
			assert.Empty(t, b.requests())
			if path != "/v1/chat/completions" {
				var flat struct {
					Error string `json:"error"`
				}
				require.NoError(t, json.Unmarshal(resp.Body, &flat))
				assert.Contains(t, flat.Error, "ollama-model")
			}
		})
	}
}

func TestBudgetUnpricedFallbackAfterFailure(t *testing.T) {
	backends := map[string]*serviceBackend{"paid": {status: 503, wire: []string{"chat_completions"}}, "unknown": {status: 200, wire: []string{"chat_completions"}}}
	s := budgetNode(t, []string{"paid", "unknown"}, backends)
	raw := budgetKey(t, s, 1, 0)
	require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "paid", Model: "paid-model", InputCostPerToken: 0.001}))
	resp := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", raw, map[string]any{"model": "claude", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	assert.Equal(t, 503, resp.Code, string(resp.Body))
	assert.NotEmpty(t, backends["paid"].requests())
	assert.Empty(t, backends["unknown"].requests())
	state := s.access.GetKeySpend("budget-key")
	require.NotNil(t, state)
	assert.Zero(t, state.ReservedMicro)
}

func TestBudgetExternalEndpointUpgrade(t *testing.T) {
	for _, tc := range []struct {
		doc       string
		key, team float64
	}{
		{"../../CHANGELOG.md", 1, 0},
		{"../../docs/configuration.md", 0, 1},
	} {
		t.Run(tc.doc, func(t *testing.T) {
			doc, err := os.ReadFile(tc.doc)
			require.NoError(t, err)
			assert.Contains(t, strings.Join(strings.Fields(string(doc)), " "), "without a managed lifecycle now receive 403 `model_unpriced` until an operator")
			_, command, found := strings.Cut(string(doc), "curl -X POST http://localhost:9090/zzrouter/v1/pricing/overrides")
			require.True(t, found, "upgrade note must give the exact API call")
			assert.Contains(t, command, "X-API-Key: $ZZROUTER_ADMIN_API_KEY")
			_, payload, found := strings.Cut(command, "-d '")
			require.True(t, found)
			payload, _, found = strings.Cut(payload, "'")
			require.True(t, found)
			var override map[string]any
			require.NoError(t, json.Unmarshal([]byte(payload), &override))
			assert.Equal(t, "my-external-provider", override["provider"])
			assert.Equal(t, "*", override["model"])
			assert.Equal(t, float64(0), override["input_cost_per_1m"])
			assert.Equal(t, float64(0), override["output_cost_per_1m"])
			b := &serviceBackend{status: 200, wire: []string{"chat_completions"}}
			s := budgetNode(t, []string{"my-external-provider"}, map[string]*serviceBackend{"my-external-provider": b})
			raw := budgetKey(t, s, tc.key, tc.team)
			body := map[string]any{"model": "claude", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			resp := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", raw, body)
			require.Equal(t, 403, resp.Code, string(resp.Body))
			assert.Contains(t, string(resp.Body), `"code":"model_unpriced"`)
			assert.Empty(t, b.requests())
			overrideRouter := gin.New()
			NewPricingController(s.model.Pricing).RegisterRoutes(overrideRouter.Group("/zzrouter/v1", s.auth.adminAuthMiddleware()))
			req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/pricing/overrides", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", TestAdminKey)
			w := httptest.NewRecorder()
			overrideRouter.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code, w.Body.String())
			resp = makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", raw, body)
			require.Equal(t, 200, resp.Code, string(resp.Body))
			assert.Len(t, b.requests(), 1)
		})
	}
}

type budgetCountingResolver struct {
	resolver.Resolver
	calls atomic.Int64
}

func (r *budgetCountingResolver) Resolve(ctx context.Context, ref resolver.Reference) (*resolver.Resolved, error) {
	r.calls.Add(1)
	return r.Resolver.Resolve(ctx, ref)
}

func TestBudgetResolutionBeforeRPM(t *testing.T) {
	doc, err := os.ReadFile("../../docs/security.md")
	require.NoError(t, err)
	assert.Contains(t, strings.Join(strings.Fields(string(doc)), " "), "Model resolution, including catalog population, runs before RPM/TPM checks, so a caller over its RPM limit still incurs a resolution.")
	b := &serviceBackend{status: 200, wire: []string{"chat_completions"}}
	s := budgetNode(t, []string{"priced"}, map[string]*serviceBackend{"priced": b})
	raw := budgetKey(t, s, 1, 0)
	resp := makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/keys/budget-key", TestAdminKey, map[string]any{"rpm_limit": 1})
	require.Equal(t, 200, resp.Code, string(resp.Body))
	require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "priced", Model: "priced-model"}))
	counter := &budgetCountingResolver{Resolver: s.model.Resolver}
	s.model.Resolver = counter
	body := map[string]any{"model": "claude", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for _, status := range []int{200, 429} {
		resp = makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", raw, body)
		require.Equal(t, status, resp.Code, string(resp.Body))
	}
	assert.EqualValues(t, 2, counter.calls.Load(), "an RPM-denied caller still incurs resolution")
	assert.Len(t, b.requests(), 1)
}

func TestBudgetSimpleRoutesComplete(t *testing.T) {
	for _, path := range []string{
		"/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/translations",
		"/v1/images/generations", "/v1/images/edits", "/v1/images/variations",
		"/v1/rerank", "/v1/embeddings", "/v1/moderations",
	} {
		t.Run(path, func(t *testing.T) {
			b := &serviceBackend{status: 200, wire: []string{"chat_completions", "embeddings", "rerank"}}
			if path == "/v1/audio/speech" {
				b.contentType, b.reply = "audio/mpeg", "audio bytes"
			}
			s := budgetNode(t, []string{"external"}, map[string]*serviceBackend{"external": b})
			raw := budgetKey(t, s, 1, 0)
			s.model.Resolver = staticModelResolver{provider: "external"}
			require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "external", Model: "external-model"}))
			prior := otel.GetMeterProvider()
			reader := metricSDK.NewManualReader()
			provider := metricSDK.NewMeterProvider(metricSDK.WithReader(reader))
			otel.SetMeterProvider(provider)
			t.Cleanup(func() { otel.SetMeterProvider(prior); require.NoError(t, provider.Shutdown(t.Context())) })
			bridge := NewInferenceLogBridge(inferencelog.NewStore(8, 0), "node", false, s.model.Pricing)
			bridge.SetAccessControl(s.access)
			llm.SetInferenceLogHook(bridge)
			t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
			c, w := buildMultipartRequest(t, map[string]string{"model": "external-model"}, "file", []byte("file bytes"), false)
			if path != "/v1/audio/transcriptions" && path != "/v1/audio/translations" && path != "/v1/images/edits" && path != "/v1/images/variations" {
				c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"external-model","input":"hi","prompt":"hi","query":"hi","documents":["hi"]}`))
				c.Request.Header.Set("Content-Type", "application/json")
			}
			c.Request.URL.Path = path
			c.Request.Header.Set("X-API-Key", raw)
			s.engine.ServeHTTP(w, c.Request)
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Len(t, b.requests(), 1)
			entries := bridge.store.Query(inferencelog.QueryFilter{Limit: 8})
			require.Len(t, entries, 1, "exactly one completion must reach the bridge")
			assert.Equal(t, "external", entries[0].App)
			assert.Equal(t, "budget-key", entries[0].KeyID)
			assert.Equal(t, "budget-team", entries[0].TeamID)
			state := s.access.GetKeySpend("budget-key")
			require.NotNil(t, state)
			assert.EqualValues(t, 1, state.RequestCount, "completion must settle the attributed reservation")
			assert.Zero(t, state.ReservedMicro)
			var metrics metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(t.Context(), &metrics))
			found := false
			for _, scope := range metrics.ScopeMetrics {
				for _, metric := range scope.Metrics {
					if metric.Name == "zz.proxy.inflight.requests.metric" {
						found = true
						data, ok := metric.Data.(metricdata.Sum[int64])
						require.True(t, ok)
						require.NotEmpty(t, data.DataPoints)
						for _, point := range data.DataPoints {
							assert.Zero(t, point.Value, "completed request must leave no in-flight gauge")
						}
					}
				}
			}
			assert.True(t, found, "the route must exercise the in-flight gauge")
		})
	}
}

func TestBudgetOllamaMixedChain(t *testing.T) {
	for _, path := range []string{"/api/chat", "/api/generate", "/api/embed", "/api/embeddings"} {
		for _, priced := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/priced=%t", path, priced), func(t *testing.T) {
				backends := map[string]*serviceBackend{
					"other":  {status: 200, wire: []string{"chat_completions"}},
					"ollama": {status: 200, wire: []string{"chat_completions"}, reply: `{"model":"known","done":true,"prompt_eval_count":2,"eval_count":1}`},
				}
				s := budgetNode(t, []string{"other", "ollama"}, backends)
				raw := budgetKey(t, s, 1, 0)
				require.NoError(t, s.model.Groups.Set("claude", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{
					{Name: "non-ollama", App: "other", Model: "other-model", Priority: 1},
					{Name: "unknown", App: "ollama", Model: "unknown", Priority: 2},
					{Name: "known", App: "ollama", Model: "known", Priority: 3},
				}}))
				require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "other", Model: "other-model"}))
				if priced {
					require.NoError(t, s.model.Pricing.SetOverride(pricing.Override{Provider: "ollama", Model: "known"}))
				}
				resp := makeAuthRequest(t, s, http.MethodPost, path, raw, map[string]any{"model": "claude", "stream": false, "messages": []any{}, "prompt": "hi", "input": "hi"})
				assert.Empty(t, backends["other"].requests(), "provider scope must precede pricing")
				if priced {
					require.Equal(t, 200, resp.Code, string(resp.Body))
					hits := backends["ollama"].requests()
					require.Len(t, hits, 1)
					assert.Equal(t, path, hits[0].Path)
					assert.Contains(t, string(hits[0].Body), `"model":"known"`)
				} else {
					require.Equal(t, 403, resp.Code, string(resp.Body))
					var flat struct {
						Error string `json:"error"`
					}
					require.NoError(t, json.Unmarshal(resp.Body, &flat))
					assert.Contains(t, flat.Error, "/zzrouter/v1/pricing/overrides")
					assert.Empty(t, backends["ollama"].requests())
				}
				assert.EqualValues(t, 1, s.model.Pricing.Misses()["ollama/unknown"])
				assert.Zero(t, s.model.Pricing.Misses()["other/other-model"])
			})
		}
	}
}
