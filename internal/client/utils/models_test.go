package client

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestModelMetadata_GetModifiedTime(t *testing.T) {
	now := time.Now()
	earlier := now.Add(-1 * time.Hour)

	tests := []struct {
		name     string
		model    ModelMetadata
		expected time.Time
	}{
		{
			name: "prefers ModifiedAt over Modified",
			model: ModelMetadata{
				ModifiedAt: now,
				Modified:   earlier,
			},
			expected: now,
		},
		{
			name: "falls back to Modified when ModifiedAt is zero",
			model: ModelMetadata{
				ModifiedAt: time.Time{},
				Modified:   earlier,
			},
			expected: earlier,
		},
		{
			name: "returns zero time when both are zero",
			model: ModelMetadata{
				ModifiedAt: time.Time{},
				Modified:   time.Time{},
			},
			expected: time.Time{},
		},
		{
			name: "returns ModifiedAt even when it's older",
			model: ModelMetadata{
				ModifiedAt: earlier,
				Modified:   now,
			},
			expected: earlier,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.model.GetModifiedTime()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestModelMetadata_GetFormat(t *testing.T) {
	tests := []struct {
		name     string
		model    ModelMetadata
		expected string
	}{
		{
			name: "format from details takes precedence",
			model: ModelMetadata{
				Details: map[string]any{"format": "gguf"},
				Format:  "safetensors",
			},
			expected: "gguf",
		},
		{
			name: "falls back to Format field when details.format is empty",
			model: ModelMetadata{
				Details: map[string]any{"format": ""},
				Format:  "safetensors",
			},
			expected: "safetensors",
		},
		{
			name: "falls back to Format field when details is nil",
			model: ModelMetadata{
				Details: nil,
				Format:  "mlx",
			},
			expected: "mlx",
		},
		{
			name: "returns empty string when both are empty",
			model: ModelMetadata{
				Details: nil,
				Format:  "",
			},
			expected: "",
		},
		{
			name: "handles non-string format in details",
			model: ModelMetadata{
				Details: map[string]any{"format": 123},
				Format:  "gguf",
			},
			expected: "gguf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.model.GetFormat()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestModelMetadata_GetQuantization(t *testing.T) {
	tests := []struct {
		name     string
		model    ModelMetadata
		expected string
	}{
		{
			name: "quantization from details takes precedence",
			model: ModelMetadata{
				Details:      map[string]any{"quantization_level": "Q4_K_M"},
				Quantization: "Q8_0",
			},
			expected: "Q4_K_M",
		},
		{
			name: "falls back to Quantization field when details is empty",
			model: ModelMetadata{
				Details:      map[string]any{"quantization_level": ""},
				Quantization: "Q5_K_S",
			},
			expected: "Q5_K_S",
		},
		{
			name: "falls back to Quantization field when details is nil",
			model: ModelMetadata{
				Details:      nil,
				Quantization: "Q8_0",
			},
			expected: "Q8_0",
		},
		{
			name: "returns empty string when both are empty",
			model: ModelMetadata{
				Details:      nil,
				Quantization: "",
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.model.GetQuantization()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestModelRegistryResponse_Data(t *testing.T) {
	models := []ModelMetadata{
		{Name: "model1"},
		{Name: "model2"},
	}
	resp := ModelRegistryResponse{Data: models, Total: 2}
	assert.Equal(t, models, resp.Data)
	assert.Equal(t, 2, resp.Total)
}

func TestModelMetadata_Fields(t *testing.T) {
	now := time.Now()

	model := ModelMetadata{
		Name:         "llama3:8b",
		Model:        "llama3:8b",
		ModifiedAt:   now,
		Size:         4_000_000_000,
		Digest:       "sha256:abc123",
		Details:      map[string]any{"format": "gguf"},
		Node:         "localhost",
		FullPath:     "/models/llama3/8b",
		Format:       "gguf",
		Quantization: "Q4_K_M",
		SourceRepo:   "huggingface",
		SourceID:     "meta-llama/Meta-Llama-3-8B",
		AssignedApp:  "llama.cpp",
		Modified:     now,
		AddedAt:      now,
	}

	assert.Equal(t, "llama3:8b", model.Name)
	assert.Equal(t, int64(4_000_000_000), model.Size)
	assert.Equal(t, "localhost", model.Node)
	assert.Equal(t, "huggingface", model.SourceRepo)
	assert.Equal(t, "llama.cpp", model.AssignedApp)
}

func TestDeleteRegistryResponse_Fields(t *testing.T) {
	resp := DeleteRegistryResponse{
		Deleted: 3,
		Errors:  []string{"error1", "error2"},
	}

	assert.Equal(t, 3, resp.Deleted)
	assert.Len(t, resp.Errors, 2)
	assert.Equal(t, "error1", resp.Errors[0])
}

func TestLoadModelResponse_Fields(t *testing.T) {
	resp := LoadModelResponse{
		InstanceID: "abc123",
		Model:      "llama3:8b",
		App:        "ollama",
		Port:       11434,
		Status:     "running",
		Node:       "localhost",
	}

	assert.Equal(t, "abc123", resp.InstanceID)
	assert.Equal(t, "llama3:8b", resp.Model)
	assert.Equal(t, "ollama", resp.App)
	assert.Equal(t, 11434, resp.Port)
	assert.Equal(t, "running", resp.Status)
	assert.Equal(t, "localhost", resp.Node)
}

// A filter value reaches the server as typed: a variant's name carries a
// "+", which a hand-built query string turned into a space.
func TestListModelsEncodesTheFilter(t *testing.T) {
	var got url.Values
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))

	_, err := c.ListModels(ModelFilter{Node: "*", Provider: "llamacpp", Model: "Qwen3.8-27B-Q8_0+agent", Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"provider": {"llamacpp"}, "model": {"Qwen3.8-27B-Q8_0+agent"}, "refresh": {"true"}}
	if got.Encode() != want.Encode() {
		t.Errorf("query = %v, want %v (\"*\" and empty fields are not sent)", got, want)
	}
}

// Every query value the client sends reaches the server as typed, a node
// name with reserved characters included.
func TestQueryValuesReachTheServerAsTyped(t *testing.T) {
	const node = "rack a&b+c"
	var got url.Values
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	calls := map[string]func(){
		"StopAllDeployments": func() { _ = c.StopAllDeployments(node) },
		"GetPreflight":       func() { _, _ = c.GetPreflight("vllm", node) },
		"GetInstallPlan":     func() { _, _ = c.GetInstallPlan("vllm", node) },
	}
	for name, call := range calls {
		got = nil
		call()
		if got.Get("node") != node || len(got) != 1 {
			t.Errorf("%s sent %v, want node=%q only", name, got, node)
		}
	}

	_, _ = c.GetNodesWithRefresh("name,os&x", true)
	if got.Get("fields") != "name,os&x" || got.Get("refresh") != "true" {
		t.Errorf("GetNodesWithRefresh sent %v", got)
	}
}
