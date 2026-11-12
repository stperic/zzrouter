package client

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryTransportPreservesEnvelopeAndNativeFields(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-key", r.Header.Get("X-API-Key"))
		switch r.URL.Path {
		case apipath.RegistriesHuggingFaceVariants:
			assert.Equal(t, "org/model", r.URL.Query().Get("id"))
			assert.Equal(t, "llamacpp", r.URL.Query().Get("provider"))
			_, _ = w.Write([]byte(`{"data":{"model_id":"org/model","variants":[{"file":"model-00001-of-00002.gguf","shards":2,"size_bytes":24,"recommended":true}],"features":[{"feature":"vision","file":"mmproj.gguf","size_bytes":6}]}}`))
		case apipath.Search:
			assert.Equal(t, "A+B", r.URL.Query().Get("q"))
			assert.Equal(t, "OR", r.URL.Query().Get("logic"))
			_, _ = w.Write([]byte(`{"data":{"type":"models","provider":"cloud","results":[{"id":"opaque-id","capabilities":{"vision":true},"discovery":{"provider":"cloud","api_id":"actual-model","display_name":"Friendly model"}}],"total":1,"is_total_exact":false,"pagination":{"limit":2,"offset":0,"has_next":true}}}`))
		case apipath.RegistriesOllamaTags:
			_, _ = w.Write([]byte(`{"data":{"model_id":"qwen3","tags":[{"name":"cloud","is_cloud":true,"context":"128K"}]}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	variants, err := c.GetHuggingFaceVariants(context.Background(), "org/model", "llamacpp")
	require.NoError(t, err)
	require.Len(t, variants.Variants, 1)
	assert.Equal(t, 2, variants.Variants[0].Shards)
	require.Len(t, variants.Features, 1)
	assert.Equal(t, "vision", variants.Features[0].Feature)
	page, err := c.SearchModels(context.Background(), metadata.SearchQuery{Query: "A+B", Provider: "cloud", Limit: 2, Logic: "OR"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "actual-model", page.Results[0].Discovery.APIID)
	assert.Equal(t, "opaque-id", page.Results[0].Native["id"])
	assert.Equal(t, map[string]any{"vision": true}, page.Results[0].Native["capabilities"])
	assert.False(t, page.IsTotalExact)
	assert.True(t, page.Pagination.HasNext)
	tags, err := c.GetOllamaTags(context.Background(), "qwen3")
	require.NoError(t, err)
	require.Len(t, tags.Tags, 1)
	assert.True(t, tags.Tags[0].IsCloud)
}

func TestDiscoveryCancellationReachesHTTP(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	c, _ := newTestClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.SearchModels(ctx, metadata.SearchQuery{}); done <- err }()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	<-cancelled
}

func TestDeploymentTransportPreservesFeatureChoiceAndJobIDs(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, apipath.Deployments, r.URL.Path)
		var req DeployRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.NotNil(t, req.Features)
		assert.Empty(t, *req.Features)
		assert.Equal(t, "llamacpp", req.Provider)
		assert.Equal(t, "affected", req.Restart)
		assert.Equal(t, []string{"a", "b"}, req.Nodes)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"data":{"id":"d","nodes":[{"node":"a","job_id":"j1"},{"node":"b","job_id":"j2"}]}}`))
	}))
	empty := []string{}
	deployment, err := c.Deploy(context.Background(), DeployRequest{Model: "org/model", Nodes: []string{"a", "b"}, Provider: "llamacpp", Features: &empty, Restart: "affected"})
	require.NoError(t, err)
	assert.Equal(t, "d", deployment.ID)
	assert.Equal(t, "j2", deployment.Nodes[1].JobID)
}

func TestDiscoveryTransportReportsMissingServerContract(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"results":[{"id":"model"}]}}`))
	}))
	_, err := c.SearchModels(context.Background(), metadata.SearchQuery{})
	require.ErrorContains(t, err, "missing discovery metadata")
}
