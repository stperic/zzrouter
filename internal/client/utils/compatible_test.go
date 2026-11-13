package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompatibleNodesUsesCanonicalModelQuery(t *testing.T) {
	calls := 0
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/zzrouter/v1/nodes/compatible", r.URL.Path)
		assert.Equal(t, "m+variant", r.URL.Query().Get("model"))
		assert.Equal(t, "huggingface", r.URL.Query().Get("registry"))
		assert.Equal(t, "vllm", r.URL.Query().Get("provider"))
		_, err := w.Write([]byte(`{"data":[],"total":0,"metadata":{"assessments":[{"node":"worker","status":"unknown","reason":"missing size"}]}}`))
		require.NoError(t, err)
	}))
	nodes, err := client.GetCompatibleNodes(context.Background(), "m+variant", "huggingface", "vllm")
	require.NoError(t, err)
	assert.Empty(t, nodes.Data)
	require.Len(t, nodes.Metadata.Assessments, 1)
	assert.Equal(t, "unknown", nodes.Metadata.Assessments[0].Status)
	assert.Equal(t, 1, calls)
}
