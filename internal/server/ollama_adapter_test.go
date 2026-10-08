package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stretchr/testify/require"
)

func TestOllamaAdapter_ListEntry_CoreFields(t *testing.T) {
	a := NewOllamaAdapter()
	modified := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		input *cache.CachedModel
		want  OllamaTagEntry
	}{
		{
			name: "minimal model",
			input: &cache.CachedModel{
				Name:     "llama3",
				Model:    "llama3",
				Size:     1_000_000_000,
				Modified: modified,
				Digest:   "sha256:abc123",
			},
			want: OllamaTagEntry{
				Name:       "llama3",
				Model:      "llama3",
				Size:       1_000_000_000,
				ModifiedAt: modified,
				Digest:     "sha256:abc123",
			},
		},
		{
			name: "tagged model with details in Details map",
			input: &cache.CachedModel{
				Name:     "llama3:8b",
				Model:    "llama3:8b",
				Size:     5_400_000_000,
				Modified: modified,
				Digest:   "sha256:def456",
				Format:   "gguf",
				Details: map[string]any{
					"family":         "llama",
					"parameter_size": "8B",
					"quant_level":    "Q4_K_M",
				},
			},
			want: OllamaTagEntry{
				Name:       "llama3:8b",
				Model:      "llama3:8b",
				Size:       5_400_000_000,
				ModifiedAt: modified,
				Digest:     "sha256:def456",
				Details: OllamaModelDetails{
					Format:        "gguf",
					Family:        "llama",
					ParameterSize: "8B",
					QuantLevel:    "Q4_K_M",
				},
			},
		},
		{
			name: "details in Extra map (legacy path)",
			input: &cache.CachedModel{
				Name:     "mistral:7b",
				Model:    "mistral:7b",
				Modified: modified,
				Extra: map[string]any{
					"format":         "gguf",
					"family":         "mistral",
					"parameter_size": "7B",
					"quant_level":    "Q5_K_M",
				},
			},
			want: OllamaTagEntry{
				Name:       "mistral:7b",
				Model:      "mistral:7b",
				ModifiedAt: modified,
				Details: OllamaModelDetails{
					Format:        "gguf",
					Family:        "mistral",
					ParameterSize: "7B",
					QuantLevel:    "Q5_K_M",
				},
			},
		},
		{
			name: "quantization_level (Ollama wire name) accepted",
			input: &cache.CachedModel{
				Name:     "phi:3.8b",
				Model:    "phi:3.8b",
				Modified: modified,
				Details: map[string]any{
					"quantization_level": "Q6_K",
				},
			},
			want: OllamaTagEntry{
				Name:       "phi:3.8b",
				Model:      "phi:3.8b",
				ModifiedAt: modified,
				Details:    OllamaModelDetails{QuantLevel: "Q6_K"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.ListEntry(tt.input)
			entry, ok := got.(OllamaTagEntry)
			require.True(t, ok, "ListEntry must return OllamaTagEntry, got %T", got)
			require.Equal(t, tt.want, entry)
		})
	}
}

func TestOllamaAdapter_ListEnvelope(t *testing.T) {
	a := NewOllamaAdapter()

	entries := []any{
		OllamaTagEntry{Name: "a"},
		OllamaTagEntry{Name: "b"},
		"wrong type — should be filtered", // defensive check
	}

	got := a.ListEnvelope(entries)
	resp, ok := got.(OllamaTagsResponse)
	require.True(t, ok, "ListEnvelope must return OllamaTagsResponse, got %T", got)
	require.Len(t, resp.Models, 2, "non-OllamaTagEntry elements must be dropped, not panicked on")
	require.Equal(t, "a", resp.Models[0].Name)
	require.Equal(t, "b", resp.Models[1].Name)
}

func TestOllamaAdapter_ListEnvelope_Empty(t *testing.T) {
	a := NewOllamaAdapter()
	got := a.ListEnvelope(nil)
	resp := got.(OllamaTagsResponse) //nolint:errcheck // test asserts on a known fixture shape
	require.NotNil(t, resp.Models, "Models slice must be non-nil even when empty")
	require.Len(t, resp.Models, 0)
}

func TestOllamaAdapter_ShowResponse_Nil(t *testing.T) {
	a := NewOllamaAdapter()
	require.Nil(t, a.ShowResponse(nil))
}

func TestOllamaAdapter_ShowResponse_NoRoute_PassesThroughDetails(t *testing.T) {
	a := NewOllamaAdapter()
	got := a.ShowResponse(&ShowModelResponse{
		Name: "llama3:8b",
		Details: map[string]any{
			"modelfile":  "FROM llama3",
			"parameters": "temperature 0.7",
			"template":   "{{ .Prompt }}",
		},
	})
	m, ok := got.(map[string]any)
	require.True(t, ok, "show response must be a map, got %T", got)
	require.Equal(t, "FROM llama3", m["modelfile"])
	require.Equal(t, "temperature 0.7", m["parameters"])
	require.NotContains(t, m, "zzrouter_route", "single-deployment model must NOT include route extension")
}

func TestOllamaAdapter_ShowResponse_WithRoute_EmitsExtension(t *testing.T) {
	a := NewOllamaAdapter()
	got := a.ShowResponse(&ShowModelResponse{
		Name: "llama3:8b",
		Details: map[string]any{
			"modelfile": "FROM llama3",
		},
		Route: &ModelRouteInfo{
			Strategy: "least-load",
			Replicas: 2,
			Nodes:    []string{"gpu-1", "gpu-2"},
			Group:    "llama3:8b",
		},
	})

	// Round-trip through JSON to confirm wire format is what Ollama clients
	// see. Unknown fields must not break encoding.
	buf, err := json.Marshal(got)
	require.NoError(t, err)
	s := string(buf)
	require.Contains(t, s, `"modelfile":"FROM llama3"`)
	require.Contains(t, s, `"zzrouter_route"`)
	require.Contains(t, s, `"strategy":"least-load"`)
	require.Contains(t, s, `"replicas":2`)
	require.Contains(t, s, `"nodes":["gpu-1","gpu-2"]`)
	require.False(t, strings.Contains(s, `"Route"`),
		"Route field must not leak with its Go name; only zzrouter_route is surfaced")
}
