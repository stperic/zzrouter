package protocol

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type modelListTransport string

func (body modelListTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func TestOpenAIListModelsKeepsIdentityTimestampAndMetadata(t *testing.T) {
	// Synthetic model cards cover both engines' context metadata and an absent timestamp.
	client := &http.Client{Transport: modelListTransport(`{"data":[
		{"id":"vllm-model","created":1720000000,"max_model_len":70560},
		{"id":"llama-model","meta":{"n_ctx":262144}}
	]}`)}
	p := NewOpenAIProvider("engine", 0, "openai", client)
	models, err := p.ListModels(t.Context(), NewTarget("http://engine", backend.Engine()).Ready())
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "vllm-model", models[0].Name)
	assert.Equal(t, "vllm-model", models[0].FullID)
	assert.Equal(t, time.Unix(1720000000, 0), models[0].ModifiedAt)
	assert.Equal(t, models[0].ModifiedAt.Format("2006-01-02 15:04"), models[0].Modified)
	assert.Equal(t, float64(70560), models[0].Extra["max_model_len"])
	assert.Equal(t, "llama-model", models[1].Name)
	assert.Equal(t, time.Unix(0, 0), models[1].ModifiedAt)
	assert.Equal(t, map[string]any{"n_ctx": float64(262144)}, models[1].Extra["meta"])
}
