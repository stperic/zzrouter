package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/model/pricing"
)

type overrideTestConfig struct{}

func (overrideTestConfig) IsEnabled() bool                   { return true }
func (overrideTestConfig) GetSource() string                 { return "" }
func (overrideTestConfig) GetRefreshInterval() time.Duration { return time.Hour }

func newOverrideRouter(t *testing.T) (*gin.Engine, *pricing.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	store := pricing.NewStore(overrideTestConfig{}, dir)
	require.NoError(t, store.LoadOverrides(dir+"/pricing_overrides.yaml"))

	r := gin.New()
	NewPricingController(store).RegisterRoutes(r.Group("/zzrouter/v1"))
	return r, store
}

func doOverrideReq(t *testing.T, r *gin.Engine, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var out map[string]any
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

// /pricing/overrides is a static sibling of the /pricing/:model param
// route; registering both must not shadow either.
func TestPricingOverrides_RoutesDoNotShadowModelLookup(t *testing.T) {
	r, _ := newOverrideRouter(t)

	code, body := doOverrideReq(t, r, http.MethodGet, "/zzrouter/v1/pricing/overrides", "")
	require.Equal(t, http.StatusOK, code)
	data, _ := body["data"].(map[string]any)
	require.NotNil(t, data)
	assert.Equal(t, float64(0), data["count"])

	// The param route still resolves for a real model name.
	code, _ = doOverrideReq(t, r, http.MethodGet, "/zzrouter/v1/pricing/gpt-4o", "")
	assert.Equal(t, http.StatusNotFound, code, "empty store: 404, not a routing error")
}

func TestPricingOverrides_UpsertConvertsPerMillionAndReadsBack(t *testing.T) {
	r, store := newOverrideRouter(t)

	code, body := doOverrideReq(t, r, http.MethodPost, "/zzrouter/v1/pricing/overrides",
		`{"provider":"openrouter","model":"meta-llama/llama-3.3-70b-instruct","input_cost_per_1m":0.5,"output_cost_per_1m":2,"note":"long tail"}`)
	require.Equal(t, http.StatusOK, code)

	data, _ := body["data"].(map[string]any)
	require.NotNil(t, data)
	assert.InDelta(t, 0.5, data["input_cost_per_1m"], 1e-9)
	assert.InDelta(t, 5e-7, data["input_cost_per_token"], 1e-15)
	assert.Equal(t, true, data["billed_per_token"])

	// The override is what the lookup path now returns.
	mp, ok := store.LookupByProvider("openrouter", "meta-llama/llama-3.3-70b-instruct")
	require.True(t, ok)
	assert.InDelta(t, 5e-7, mp.InputCostPerToken, 1e-15)
}

// The all-zero wildcard is the ollama-cloud shape: subscription billed,
// no per-token rate exists, and it must still register as priced so the
// traffic stops being reported as un-priced.
func TestPricingOverrides_ZeroWildcardMarksProviderNotBilledPerToken(t *testing.T) {
	r, store := newOverrideRouter(t)
	store.RecordMiss("ollama-cloud", "gpt-oss:120b")
	require.NotEmpty(t, store.Misses())

	code, body := doOverrideReq(t, r, http.MethodPost, "/zzrouter/v1/pricing/overrides",
		`{"provider":"ollama-cloud","model":"*","note":"subscription billed"}`)
	require.Equal(t, http.StatusOK, code)
	data, _ := body["data"].(map[string]any)
	assert.Equal(t, false, data["billed_per_token"])
	assert.Equal(t, true, data["applies_to_all_models"])

	mp, ok := store.LookupOverride("ollama-cloud", "any-model-at-all")
	require.True(t, ok, "wildcard must cover models never seen before")
	assert.Zero(t, mp.InputCostPerToken)
}

func TestPricingOverrides_DeleteRequiresModelAnd404sWhenAbsent(t *testing.T) {
	r, _ := newOverrideRouter(t)

	code, _ := doOverrideReq(t, r, http.MethodDelete, "/zzrouter/v1/pricing/overrides", "")
	assert.Equal(t, http.StatusBadRequest, code)

	code, _ = doOverrideReq(t, r, http.MethodDelete, "/zzrouter/v1/pricing/overrides?model=ghost", "")
	assert.Equal(t, http.StatusNotFound, code)

	_, _ = doOverrideReq(t, r, http.MethodPost, "/zzrouter/v1/pricing/overrides",
		`{"provider":"groq","model":"llama-3.3-70b-versatile","input_cost_per_1m":1}`)
	code, _ = doOverrideReq(t, r, http.MethodDelete,
		"/zzrouter/v1/pricing/overrides?model=llama-3.3-70b-versatile&provider=groq", "")
	assert.Equal(t, http.StatusOK, code)
}

func TestPricingOverrides_RejectsMissingModelAndNegativeRate(t *testing.T) {
	r, _ := newOverrideRouter(t)

	code, _ := doOverrideReq(t, r, http.MethodPost, "/zzrouter/v1/pricing/overrides",
		`{"provider":"openai","input_cost_per_1m":1}`)
	assert.Equal(t, http.StatusBadRequest, code, "model is required")

	code, _ = doOverrideReq(t, r, http.MethodPost, "/zzrouter/v1/pricing/overrides",
		`{"model":"gpt-4o","input_cost_per_1m":-1}`)
	assert.Equal(t, http.StatusBadRequest, code, "negative rate is not a discount")

	code, _ = doOverrideReq(t, r, http.MethodPost, "/zzrouter/v1/pricing/overrides",
		`{"model":"*","input_cost_per_1m":1}`)
	assert.Equal(t, http.StatusBadRequest, code, "wildcard without a provider would price everything")
}

// Overrides must survive a restart: the store reloads them from disk.
func TestPricingOverrides_PersistAcrossReload(t *testing.T) {
	r, store := newOverrideRouter(t)
	_, _ = doOverrideReq(t, r, http.MethodPost, "/zzrouter/v1/pricing/overrides",
		`{"provider":"cloudflare","model":"@cf/meta/llama-3.1-8b-instruct","input_cost_per_1m":0.3,"output_cost_per_1m":0.3}`)

	path := store.OverridesPath()
	require.NotEmpty(t, path)

	reloaded := pricing.NewStore(overrideTestConfig{}, t.TempDir())
	require.NoError(t, reloaded.LoadOverrides(path))

	mp, ok := reloaded.LookupByProvider("cloudflare", "@cf/meta/llama-3.1-8b-instruct")
	require.True(t, ok)
	assert.InDelta(t, 3e-7, mp.InputCostPerToken, 1e-15)
}
