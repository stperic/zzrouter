package search

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testTarget(endpoint, revision, token string) *backend.Resolved {
	return &backend.Resolved{Endpoint: endpoint, Revision: revision, Configured: true, Upstream: backend.Credential(&config.APIConfig{AuthType: config.AuthTypeBearer, Token: token})}
}

func TestHFSearchCardTreeShareCapturedCredential(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/models":
			_, _ = w.Write([]byte(`[{"id":"org/model","safetensors":{"total":7000000000}}]`))
		case "/api/models/org/model":
			_, _ = w.Write([]byte(`{"id":"org/model","siblings":[{"rfilename":"weights.gguf","size":0}]}`))
		case "/org/model/resolve/main/README.md":
			_, _ = w.Write([]byte("# Model\nA test model."))
		case "/api/models/org/model/tree/main":
			_, _ = w.Write([]byte(`[{"type":"file","path":"weights.gguf","size":32}]`))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
	}))
	defer upstream.Close()
	target := testTarget(upstream.URL, "r1", "fixture")
	client := NewClient(ClientOptions{Target: target})
	_, err := client.SearchModels(context.Background(), SearchParams{Search: "model"})
	require.NoError(t, err)
	card, err := NewCardCache().GetModelCard(context.Background(), ProviderHuggingFace, "org/model", target)
	require.NoError(t, err)
	siblings, err := client.EnrichSiblingsWithSizes(context.Background(), "org/model", card.Siblings)
	require.NoError(t, err)
	require.Len(t, siblings, 1)
	assert.Equal(t, float64(32), siblings[0].Size)
	assert.Equal(t, int32(4), calls.Load())
}

func TestCardCacheLateOldRevisionCannotReplaceCurrent(t *testing.T) {
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "README") {
			_, _ = w.Write([]byte("Test description"))
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "old" {
			close(oldStarted)
			<-releaseOld
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": token, "tags": []string{"original"}})
	}))
	defer upstream.Close()
	cache := NewCardCache()
	oldDone := make(chan error, 1)
	go func() {
		_, err := cache.GetModelCard(context.Background(), ProviderHuggingFace, "org/model", testTarget(upstream.URL, "r1", "old"))
		oldDone <- err
	}()
	<-oldStarted
	current := testTarget(upstream.URL, "r2", "new")
	card, err := cache.GetModelCard(context.Background(), ProviderHuggingFace, "org/model", current)
	require.NoError(t, err)
	assert.Equal(t, "new", card.Name)
	card.Tags[0] = "mutated"
	close(releaseOld)
	require.NoError(t, <-oldDone)
	card, err = cache.GetModelCard(context.Background(), ProviderHuggingFace, "org/model", current)
	require.NoError(t, err)
	assert.Equal(t, "new", card.Name)
	assert.Equal(t, "original", card.Tags[0])
}

func TestCardCancellationReachesUpstream(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(stopped) }))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := NewCardCache().GetModelCard(ctx, ProviderHuggingFace, "org/model", testTarget(upstream.URL, "r1", "fixture"))
		done <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	<-stopped
}

func TestCloudTargetPreservesNativeFieldsAndRefusesRedirect(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			return
		}
		assert.Equal(t, "/custom/models", r.URL.Path)
		assert.Equal(t, "fixture", r.URL.Query().Get("account"))
		_, _ = w.Write([]byte(`{"models":[{"id":"model","capabilities":{"vision":true},"display_name":"Friendly"}]}`))
	}))
	defer upstream.Close()
	target := testTarget(upstream.URL, "r1", "fixture")
	target.ModelsPath = "/custom/models?account=fixture"
	target.ModelsResponseKey = "models"
	models, err := SearchCloudTarget(context.Background(), target, "model", 10)
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, map[string]any{"vision": true}, models[0].(map[string]any)["capabilities"])
	target.ModelsPath = "/redirect"
	_, err = SearchCloudTarget(context.Background(), target, "", 10)
	require.Error(t, err)
	assert.Zero(t, destinationCalls.Load())
}

type catalogTransport func(*http.Request) (*http.Response, error)

func (f catalogTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOllamaTagsUseFixedOriginAndRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client := &http.Client{Transport: catalogTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, "https", r.URL.Scheme)
		assert.Equal(t, "ollama.com", r.URL.Host)
		assert.Equal(t, "/library/qwen3/tags", r.URL.Path)
		assert.Same(t, ctx, r.Context())
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`<a href="/library/qwen3:cloud">cloud</a>`))}, nil
	})}
	_, err := getOllamaModelTags(ctx, client, "qwen3")
	require.NoError(t, err)
	for _, id := range []string{"../secret", "https://other.test", "a/b", "a?token=x", "a#b", "a%2Fb", ""} {
		_, err := getOllamaModelTags(ctx, client, id)
		require.Error(t, err, id)
	}
	assert.Equal(t, 1, calls)
}
