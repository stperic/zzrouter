package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// Synthetic cases extend the captured OpenRouter shape to exercise free and sub-microUSD requests.
func usageResponse(cost, upstream float64) string {
	return fmt.Sprintf(`{"id":"gen-test","model":"gpt-4o","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":19,"completion_tokens":3,"cost":%g,"is_byok":%t,"cost_details":{"upstream_inference_cost":%g},"prompt_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}`, cost, upstream > 0, upstream)
}

func usageStream(body string) string {
	return "data: " + body + "\n\ndata: [DONE]\n\n"
}

type usageBridgeHook struct {
	bridge *InferenceLogBridge
	calls  int
	data   llm.InferenceLogData
}

func (h *usageBridgeHook) OnInferenceComplete(data llm.InferenceLogData) {
	h.calls++
	h.data = data
	h.bridge.OnInferenceComplete(data)
}

func TestCostTruthResponseLogLedger(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, tc := range []struct {
		name, source         string
		cost, upstream, want float64
		legacy               bool
	}{
		{name: "paid", cost: 0.000001989, want: 0.000001989, source: "provider"},
		{name: "standard zero", source: "provider"},
		{name: "sub-micro", cost: 0.0000004, want: 0.0000004, source: "provider"},
		{name: "BYOK", upstream: 0.0021, want: 0.0021, source: "provider"},
		{name: "account plus upstream", cost: 0.001, upstream: 0.0021, want: 0.0031, source: "provider"},
		{name: "extension zero", legacy: true, want: 0.000077, source: "zzrouter"},
	} {
		for _, stream := range []bool{false, true} {
			for _, group := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/group=%v", tc.name, stream, group), func(t *testing.T) {
					access, _ := newGatewayWithEnforcer(t)
					b := bridgeWithPricing(t, map[string]pricing.ModelPricing{"gpt-4o": bridgeGPT4o})
					b.SetAccessControl(access)
					hook := &usageBridgeHook{bridge: b}
					llm.SetInferenceLogHook(hook)
					t.Cleanup(func() { llm.SetInferenceLogHook(nil) })

					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
					q := quota.QuotaConfig{SpendLimit: 10, DefaultMaxTokens: 50}
					setAccessContext(c, &AccessContext{
						Key:  &KeyPrincipal{ID: "alice", IsVirtual: true, Quotas: q},
						Team: &TeamPrincipal{ID: "eng", Quotas: q},
					})
					require.True(t, access.Enforce(c, "gpt-4o", paidPreparation))
					rec := llm.NewInferenceRecorder(t.Context(), "gpt-4o", "openrouter")
					rec.SetKeyID("alice")
					rec.SetTeamID("eng")
					access.MirrorReservationIDToRecorder(c, rec)
					body := usageResponse(tc.cost, tc.upstream)
					if tc.legacy {
						body = strings.Replace(body, `,"cost":0`, "", 1)
						body = body[:len(body)-1] + `,"x_gateway":{"cost":0}}`
					}
					contentType := "application/json"
					if stream {
						body, contentType = usageStream(body), "text/event-stream"
					}
					w := httptest.NewRecorder()
					if group {
						resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
						defer resp.Body.Close()
						meta := wire.RoutingMetadata{Provider: "openrouter", Deployment: "gpt-4o", GroupName: "chat", InjectUsage: true}
						result := wire.CommitWithRouting(t.Context(), w, resp, meta, rec, s.inference.normalizers, newChainServerDeps(s).BufferedShape(meta, rec))
						require.Equal(t, wire.Completed, result.Outcome)
					} else {
						client := &http.Client{Transport: streamTestTransport{body: body, contentType: contentType}}
						pc := NewProxyClient(client, nil, s.inference.normalizers, nil, func() bool { return true }, nil, nil)
						ctx := context.WithValue(t.Context(), CtxKeyInferenceRecorder, rec)
						req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
						pc.ForwardToBackend(w, req, "openrouter", "https://fixture.invalid/v1/chat/completions", backend.Engine(), []byte(`{"model":"gpt-4o"}`))
					}
					assert.Equal(t, 1, hook.calls)
					entries := b.store.Query(inferencelog.QueryFilter{Limit: 10})
					require.Len(t, entries, 1)
					assert.InDelta(t, tc.want, entries[0].Cost, 1e-15)
					assert.Equal(t, tc.source, entries[0].CostSource)
					assert.EqualValues(t, 19, entries[0].TokensIn)
					assert.EqualValues(t, 3, entries[0].TokensOut)
					assert.EqualValues(t, 2, entries[0].TokensCached)
					assert.Empty(t, b.pricingStore.Misses())
					for _, state := range []*quota.SpendState{access.GetKeySpend("alice"), access.GetTeamSpend("eng")} {
						require.NotNil(t, state)
						assert.Equal(t, quota.USDToMicro(tc.want), state.SpendMicro)
						assert.Zero(t, state.ReservedMicro)
						assert.EqualValues(t, 1, state.RequestCount)
						assert.EqualValues(t, 19, state.TokensIn)
						assert.EqualValues(t, 3, state.TokensOut)
					}
					// Stream metadata precedes settlement, so pricing fallback is not yet available.
					if !stream || !tc.legacy {
						assertUsageClean(t, w.Body.String(), stream, tc.want, tc.source, true)
					}
					if !stream {
						assert.Contains(t, string(hook.data.ResponseBody), `"cost":`)
						assert.Equal(t, fmt.Sprint(w.Body.Len()), w.Header().Get("Content-Length"))
					}
				})
			}
		}
	}
}

func assertUsageClean(t *testing.T, body string, stream bool, cost float64, source string, metadata bool) {
	t.Helper()
	if stream {
		body = strings.TrimPrefix(strings.Split(body, "\n")[0], "data: ")
	}
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &obj))
	usage := obj["usage"].(map[string]any)
	for _, key := range []string{"cost", "cost_details", "is_byok"} {
		assert.NotContains(t, usage, key)
	}
	assert.Equal(t, map[string]any{"cached_tokens": float64(2)}, usage["prompt_tokens_details"])
	if metadata {
		if stream {
			assert.Equal(t, cost, usage["zz_cost_usd"])
			assert.Equal(t, source, usage["zz_cost_source"])
		} else {
			zz := obj["zzrouter"].(map[string]any)
			assert.Equal(t, cost, zz["cost_usd"])
			assert.Equal(t, source, zz["cost_source"])
		}
	} else {
		assert.NotContains(t, obj, "zzrouter")
		assert.NotContains(t, usage, "zz_cost_usd")
		assert.NotContains(t, usage, "zz_cost_source")
	}
}

func TestClusterUsageSurvivesUntilCoordinator(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, stream := range []bool{false, true} {
		for _, output := range []string{"metadata", "disabled", "suppressed"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, output), func(t *testing.T) {
				const cost = 0.000001989
				require.NoError(t, s.role.Set(t.Context(), role.RoleWorker, "worker relay"))
				body := usageResponse(cost, 0)
				if stream {
					body = usageStream(body)
				}
				worker := httptest.NewRecorder()
				relayUsageFixture(t, s, worker, body, stream, wire.RoutingMetadata{SuppressBodyMetadata: true}, nil)
				assert.Equal(t, body, worker.Body.String())
				require.NoError(t, s.role.Set(t.Context(), role.RoleCoordinator, "coordinator relay"))
				rec := llm.NewInferenceRecorder(t.Context(), "gpt-4o", "openrouter")
				w := httptest.NewRecorder()
				meta := wire.RoutingMetadata{Provider: "openrouter", InjectUsage: output == "metadata", SuppressBodyMetadata: output != "metadata"}
				relayUsageFixture(t, s, w, worker.Body.String(), stream, meta, rec)
				assert.Equal(t, cost, rec.Snapshot().Cost)
				assert.Equal(t, "provider", rec.Snapshot().CostSource)
				assertUsageClean(t, w.Body.String(), stream, cost, "provider", output == "metadata")
				if stream && output == "suppressed" {
					usageOnly := `{"choices":[],"usage":{"prompt_tokens":19,"completion_tokens":3,"cost":0.000001989}}`
					meta.SuppressUsageFrame = true
					w = httptest.NewRecorder()
					rec = llm.NewInferenceRecorder(t.Context(), "m", "openrouter")
					relayUsageFixture(t, s, w, usageStream(usageOnly), true, meta, rec)
					assert.NotContains(t, w.Body.String(), `"usage"`)
					assert.Contains(t, w.Body.String(), "[DONE]")
					assert.Equal(t, cost, rec.Snapshot().Cost)
				}
			})
		}
	}
}

func relayUsageFixture(t *testing.T, s *Server, w http.ResponseWriter, body string, stream bool, meta wire.RoutingMetadata, rec *llm.InferenceRecorder) {
	t.Helper()
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	defer resp.Body.Close()
	if stream {
		resp.Header.Set("Content-Type", "text/event-stream")
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		require.Equal(t, wire.Completed, streamWithUsageInject(req, w, resp, s.inference.normalizers, "openrouter", meta, rec).Outcome)
	} else {
		require.Equal(t, wire.Completed, wire.RelayBuffered(w, resp, normalizedBody(s.inference.normalizers, "openrouter", meta, rec), rec).Outcome)
	}
}
