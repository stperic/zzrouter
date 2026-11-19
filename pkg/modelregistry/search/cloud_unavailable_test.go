package search

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stretchr/testify/require"
)

func TestCloudCatalogUnavailableNeverContactsUpstream(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(upstream.Close)
	_, err := SearchCloudTarget(t.Context(), &backend.Resolved{Endpoint: upstream.URL, ModelsPath: new("")}, "", 10)
	require.ErrorIs(t, err, ErrModelDiscoveryUnavailable)
	require.Zero(t, requests.Load())
}

func TestCloudCatalogUsesCompatiblePrefixAndExplicitOverride(t *testing.T) {
	var requested atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"fixture-model"}]}`))
	}))
	t.Cleanup(upstream.Close)
	target := &backend.Resolved{Endpoint: upstream.URL, OpenAIPath: "/compatible"}
	models, err := SearchCloudTarget(t.Context(), target, "", 10)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "/compatible/models", requested.Load())
	target.ModelsPath = new("/catalog?limit=5")
	_, err = SearchCloudTarget(t.Context(), target, "", 10)
	require.NoError(t, err)
	require.Equal(t, "/catalog?limit=5", requested.Load())
}
