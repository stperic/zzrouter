package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testConfig implements StoreConfig for tests.
type testConfig struct {
	source          string
	refreshInterval time.Duration
}

func (c *testConfig) IsEnabled() bool   { return true }
func (c *testConfig) GetSource() string { return c.source }
func (c *testConfig) GetRefreshInterval() time.Duration {
	if c.refreshInterval > 0 {
		return c.refreshInterval
	}
	return DefaultRefreshInterval
}

func newTestStore(t *testing.T, cfg StoreConfig, cacheDir string) *Store {
	t.Helper()
	return NewStore(cfg, cacheDir)
}

func sampleLiteLLMJSON() []byte {
	data := map[string]any{
		"sample_spec": map[string]any{
			"input_cost_per_token": 0.0,
			"mode":                 "chat",
		},
		"gpt-4o": map[string]any{
			"input_cost_per_token":  0.0000025,
			"output_cost_per_token": 0.00001,
			"max_input_tokens":      128000,
			"max_output_tokens":     16384,
			"litellm_provider":      "openai",
			"mode":                  "chat",
			"supports_vision":       true,
		},
		"claude-sonnet-4-20250514": map[string]any{
			"input_cost_per_token":  0.000003,
			"output_cost_per_token": 0.000015,
			"max_input_tokens":      200000,
			"max_output_tokens":     8192,
			"litellm_provider":      "anthropic",
			"mode":                  "chat",
		},
		"openai/gpt-4o-mini": map[string]any{
			"input_cost_per_token":  0.00000015,
			"output_cost_per_token": 0.0000006,
			"max_input_tokens":      128000,
			"max_output_tokens":     16384,
			"litellm_provider":      "openai",
			"mode":                  "chat",
		},
	}
	b, _ := json.Marshal(data)
	return b
}

func TestParseLiteLLM(t *testing.T) {
	models, err := parseLiteLLM(sampleLiteLLMJSON())
	require.NoError(t, err)

	// sample_spec should be skipped
	_, hasSampleSpec := models["sample_spec"]
	assert.False(t, hasSampleSpec, "sample_spec should be skipped")

	assert.Len(t, models, 3)

	gpt4o := models["gpt-4o"]
	assert.Equal(t, 0.0000025, gpt4o.InputCostPerToken)
	assert.Equal(t, 0.00001, gpt4o.OutputCostPerToken)
	assert.Equal(t, 128000, gpt4o.MaxInputTokens)
	assert.Equal(t, "openai", gpt4o.Provider)
	assert.Equal(t, "chat", gpt4o.Mode)
	assert.True(t, gpt4o.SupportsVision)
}

func TestModelPricingHelpers(t *testing.T) {
	p := ModelPricing{
		InputCostPerToken:  0.000003,
		OutputCostPerToken: 0.000015,
		MaxInputTokens:     200000,
	}

	assert.InDelta(t, 3.0, p.InputCostPer1M(), 0.001)
	assert.InDelta(t, 15.0, p.OutputCostPer1M(), 0.001)
	assert.Equal(t, 200000, p.ContextWindow())
	assert.True(t, p.HasPricing())

	empty := ModelPricing{}
	assert.False(t, empty.HasPricing())
	assert.Equal(t, 0, empty.ContextWindow())
}

func TestStoreLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"test-etag"`)
		_, _ = w.Write(sampleLiteLLMJSON())
	}))
	defer server.Close()

	cfg := &testConfig{source: server.URL}
	dir := t.TempDir()
	store := newTestStore(t, cfg, dir)

	store.refresh(context.Background())

	assert.Equal(t, 3, store.ModelCount())

	// Exact lookup
	gpt4o, ok := store.Lookup("gpt-4o")
	assert.True(t, ok)
	assert.Equal(t, "openai", gpt4o.Provider)

	// Provider-prefixed lookup
	mini, ok := store.LookupByProvider("openai", "gpt-4o-mini")
	assert.True(t, ok)
	assert.InDelta(t, 0.00000015, mini.InputCostPerToken, 1e-12)

	// Provider fallback to bare name
	claude, ok := store.LookupByProvider("anthropic", "claude-sonnet-4-20250514")
	assert.True(t, ok)
	assert.InDelta(t, 0.000003, claude.InputCostPerToken, 1e-12)

	// Miss
	_, ok = store.Lookup("nonexistent-model")
	assert.False(t, ok)
}

func TestStoreSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sampleLiteLLMJSON())
	}))
	defer server.Close()

	store := newTestStore(t, &testConfig{source: server.URL}, t.TempDir())
	store.refresh(context.Background())

	results := store.Search("gpt", "")
	assert.Len(t, results, 2) // gpt-4o and openai/gpt-4o-mini

	// Filter by provider
	openaiOnly := store.Search("", "openai")
	assert.Len(t, openaiOnly, 2) // gpt-4o and openai/gpt-4o-mini

	anthropicOnly := store.Search("", "anthropic")
	assert.Len(t, anthropicOnly, 1) // claude-sonnet-4-20250514

	// Combined: query + provider
	combined := store.Search("gpt", "openai")
	assert.Len(t, combined, 2)
}

func TestStoreDiskPersistence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sampleLiteLLMJSON())
	}))
	defer server.Close()

	dir := t.TempDir()
	cfg := &testConfig{source: server.URL}

	// First store: fetch and persist
	store1 := newTestStore(t, cfg, dir)
	store1.refresh(context.Background())
	assert.Equal(t, 3, store1.ModelCount())

	// Verify cache file exists
	cachePath := filepath.Join(dir, "pricing-cache.json")
	_, err := os.Stat(cachePath)
	require.NoError(t, err)

	// Second store: lazy-loads from disk on first read (no server needed)
	store2 := newTestStore(t, cfg, dir)
	assert.Equal(t, 3, store2.ModelCount())

	gpt4o, ok := store2.Lookup("gpt-4o")
	assert.True(t, ok)
	assert.Equal(t, "openai", gpt4o.Provider)
}

func TestConditionalFetch304(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Header.Get("If-None-Match") == `"test-etag"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"test-etag"`)
		_, _ = w.Write(sampleLiteLLMJSON())
	}))
	defer server.Close()

	store := newTestStore(t, &testConfig{source: server.URL}, t.TempDir())

	// First fetch — full download
	store.refresh(context.Background())
	assert.Equal(t, 3, store.ModelCount())
	assert.Equal(t, 1, callCount)

	// Second fetch — should get 304
	store.refresh(context.Background())
	assert.Equal(t, 2, callCount)
	assert.Equal(t, 3, store.ModelCount()) // data unchanged
}

func TestStoreStartAndShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sampleLiteLLMJSON())
	}))
	defer server.Close()

	store := NewStore(&testConfig{
		source:          server.URL,
		refreshInterval: 100 * time.Millisecond,
	}, t.TempDir())

	// No disk cache — lazy load returns 0.
	assert.Equal(t, 0, store.ModelCount())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	store.Start(ctx)

	// Start is non-blocking; wait for the background refresh to populate.
	require.Eventually(t, func() bool {
		return store.ModelCount() > 0
	}, 2*time.Second, 10*time.Millisecond)
}

// TestRefreshTickerFiresPeriodically asserts that Start drives multiple
// network fetches via the ticker, not just the initial one.
func TestRefreshTickerFiresPeriodically(t *testing.T) {
	hits := make(chan struct{}, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		_, _ = w.Write(sampleLiteLLMJSON())
	}))
	defer server.Close()

	store := NewStore(&testConfig{
		source:          server.URL,
		refreshInterval: 50 * time.Millisecond,
	}, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store.Start(ctx)

	// Wait for 3 hits: initial fetch + 2 ticker ticks.
	for i := 0; i < 3; i++ {
		select {
		case <-hits:
		case <-time.After(2 * time.Second):
			t.Fatalf("ticker did not fire refresh #%d within 2s", i+1)
		}
	}
}

func TestLazyLoad_CorruptCacheRotates(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "pricing-cache.json")

	// Write a corrupt cache file
	require.NoError(t, os.WriteFile(cachePath, []byte("{invalid json"), 0o644))

	store := newTestStore(t, &testConfig{source: "http://unused"}, dir)

	// First read triggers lazy-load. Corrupt file is rotated out of the
	// way and the store starts empty.
	assert.Equal(t, 0, store.ModelCount())

	// Original file should be gone, replaced by a .corrupt backup
	_, err := os.Stat(cachePath)
	assert.True(t, os.IsNotExist(err), "corrupt file should have been renamed")

	corruptPath := cachePath + ".corrupt"
	_, err = os.Stat(corruptPath)
	assert.NoError(t, err, ".corrupt backup should exist")
}

func TestFetchRejectsOversizedBody(t *testing.T) {
	// Server returns a body larger than maxBodySize (50MB). We verify the
	// limit mechanism works by creating a server that streams endless
	// data and confirming fetch returns within a reasonable time without
	// OOMing.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Write 51MB of zeros — more than the 50MB cap
		buf := make([]byte, 1<<20) // 1MB chunk
		for i := 0; i < 51; i++ {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := fetch(ctx, server.URL, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestFetchHandles5xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := fetch(context.Background(), server.URL, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status 503")
}

func TestTestConfigSatisfiesInterface(t *testing.T) {
	cfg := &testConfig{source: "https://example.com"}
	var _ StoreConfig = cfg // compile-time check
	assert.True(t, cfg.IsEnabled())
	assert.Equal(t, "https://example.com", cfg.GetSource())
	assert.Equal(t, DefaultRefreshInterval, cfg.GetRefreshInterval())
}
