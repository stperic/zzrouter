package server

import (
	"slices"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
)

// fakeApps builds a minimal AppsConfig with the providers used by the
// derivation tests. The returned object does not hit disk or any real
// provider runtime; runtime fields are populated to the minimum each
// kind's Validate accepts.
func fakeApps(t *testing.T) *pkgConfig.AppsConfig {
	t.Helper()
	onDemandRuntime := func(port int) *pkgConfig.AppRuntimeConfig {
		return &pkgConfig.AppRuntimeConfig{
			BasePort:  port,
			PortRange: []int{port, port + 9},
			Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "x"},
		}
	}
	caps := func(eps ...string) *pkgConfig.AppCapabilities {
		return &pkgConfig.AppCapabilities{WireEndpoints: eps}
	}
	apps := map[string]pkgConfig.ServiceConfig{
		"vllm":     {Name: "vLLM", Protocol: pkgConfig.ProtocolOpenAI, Mode: "on-demand", Runtime: onDemandRuntime(8000), Capabilities: caps("chat_completions", "completions", "embeddings", "responses")},
		"llamacpp": {Name: "llama.cpp", Protocol: pkgConfig.ProtocolOpenAI, Mode: "on-demand", Runtime: onDemandRuntime(8080), Capabilities: caps("chat_completions", "completions", "embeddings")},
		"mlx":      {Name: "MLX", Protocol: pkgConfig.ProtocolOpenAI, Mode: "on-demand", Runtime: onDemandRuntime(8090), Capabilities: caps("chat_completions")},
		"ollama":   {Name: "Ollama", Protocol: pkgConfig.ProtocolOllama, Mode: "external", Runtime: &pkgConfig.AppRuntimeConfig{Endpoint: "http://localhost:11434"}, Capabilities: caps("chat_completions", "completions", "embeddings")},
	}
	cfg := &pkgConfig.AppsConfig{}
	for name, sc := range apps {
		if err := cfg.AddApp(name, sc); err != nil {
			t.Fatalf("AddApp(%q): %v", name, err)
		}
	}
	return cfg
}

// TestDeriveCapabilities_OpenAIProtocol pins the full flag set for a
// conventional chat model served via the OpenAI protocol.
func TestDeriveCapabilities_OpenAIProtocol(t *testing.T) {
	m := &cache.CachedModel{Name: "Qwen2.5-7B-Instruct", Provider: "vllm"}
	caps := deriveCapabilities(m, fakeApps(t))

	wantTrue := []struct {
		name string
		val  bool
	}{
		{"chat", caps.Chat},
		{"completions", caps.Completions},
		{"stream", caps.Stream},
		{"tools", caps.Tools},
		{"json_mode", caps.JSONMode},
	}
	for _, w := range wantTrue {
		if !w.val {
			t.Errorf("expected %s=true, got false", w.name)
		}
	}
	if caps.Embeddings {
		t.Error("expected embeddings=false on chat model")
	}
	if caps.Vision {
		t.Error("expected vision=false on non-multimodal model")
	}
}

// TestDeriveCapabilities_OllamaProtocol pins the Ollama-specific
// differences: json_mode should be false because Ollama does not
// accept the response_format parameter.
func TestDeriveCapabilities_OllamaProtocol(t *testing.T) {
	m := &cache.CachedModel{Name: "llama3.1", Provider: "ollama"}
	caps := deriveCapabilities(m, fakeApps(t))
	if !caps.Chat || !caps.Stream || !caps.Completions {
		t.Error("expected chat, stream, completions all true")
	}
	if caps.JSONMode {
		t.Error("expected json_mode=false on ollama")
	}
}

// TestDeriveCapabilities_EmbeddingName returns an embeddings-only
// capability set so chat-only clients don't blindly send a chat
// request to an embedding backend.
func TestDeriveCapabilities_EmbeddingName(t *testing.T) {
	cases := []string{
		"text-embedding-3-small",
		"bge-small-en",
		"e5-large-v2",
		"nomic-embed-text",
	}
	for _, name := range cases {
		m := &cache.CachedModel{Name: name, Provider: "vllm"}
		caps := deriveCapabilities(m, fakeApps(t))
		if !caps.Embeddings {
			t.Errorf("%s: expected embeddings=true", name)
		}
		if caps.Chat || caps.Completions || caps.Tools {
			t.Errorf("%s: expected chat/completions/tools all false for embedding model", name)
		}
	}
}

// TestDeriveCapabilities_VisionName flips vision=true without
// disabling chat — vision models are almost always chat-capable.
func TestDeriveCapabilities_VisionName(t *testing.T) {
	cases := []string{"Qwen2-VL-7B", "llava-1.6-7b", "gpt-4o-vision-preview"}
	for _, name := range cases {
		m := &cache.CachedModel{Name: name, Provider: "vllm"}
		caps := deriveCapabilities(m, fakeApps(t))
		if !caps.Vision {
			t.Errorf("%s: expected vision=true", name)
		}
		if !caps.Chat {
			t.Errorf("%s: expected chat=true on vision model", name)
		}
	}
}

// TestDeriveCapabilities_UnknownProvider falls back to chat-only so
// an unregistered model still exposes the most-used endpoint.
func TestDeriveCapabilities_UnknownProvider(t *testing.T) {
	m := &cache.CachedModel{Name: "mystery", Provider: "not-registered"}
	caps := deriveCapabilities(m, fakeApps(t))
	if !caps.Chat {
		t.Error("expected chat=true fallback for unknown provider")
	}
	if caps.Tools || caps.JSONMode || caps.Completions {
		t.Error("expected unknown provider to report only chat+stream")
	}
}

// TestDeriveCapabilities_NilAppsConfig covers the bootstrap path
// where apps.yaml has not yet been loaded.
func TestDeriveCapabilities_NilAppsConfig(t *testing.T) {
	m := &cache.CachedModel{Name: "qwen", Provider: "vllm"}
	caps := deriveCapabilities(m, nil)
	if !caps.Chat {
		t.Error("expected chat=true when appsConfig is nil")
	}
}

// TestDeriveCapabilities_NilModel guards the hot path: handleModels
// must not panic on a nil cache.CachedModel slot.
func TestDeriveCapabilities_NilModel(t *testing.T) {
	caps := deriveCapabilities(nil, fakeApps(t))
	if !caps.Chat || !caps.Stream {
		t.Error("nil model should still get the chat-only baseline")
	}
}

// TestDeriveCapabilities_EncoderOnlyArchitecture pins the rule that the
// provider-reported architecture decides chat capability even when the
// model name follows no embedding convention. all-minilm is the case
// that motivated it: it reports family "bert" and was being advertised
// as a chat model.
func TestDeriveCapabilities_EncoderOnlyArchitecture(t *testing.T) {
	cases := []struct {
		name           string
		model          *cache.CachedModel
		wantEmbeddings bool
		wantChat       bool
		wantRerank     bool
	}{
		{
			name: "bert family is not a chat model",
			model: &cache.CachedModel{
				Name: "all-minilm:latest", Provider: "ollama",
				Details: map[string]any{"family": "bert"},
			},
			wantEmbeddings: true,
		},
		{
			name: "families list is consulted too",
			model: &cache.CachedModel{
				Name: "unconventional-name:latest", Provider: "ollama",
				Details: map[string]any{"families": []any{"nomic-bert"}},
			},
			wantEmbeddings: true,
		},
		{
			name: "decoder family keeps chat",
			model: &cache.CachedModel{
				Name: "qwen2.5:0.5b", Provider: "ollama",
				Details: map[string]any{"family": "qwen2"},
			},
			wantChat: true,
		},
		{
			name:           "absent details falls back to the name",
			model:          &cache.CachedModel{Name: "nomic-embed-text", Provider: "ollama"},
			wantEmbeddings: true,
		},
		{
			name: "rerank wins over an encoder-only architecture",
			model: &cache.CachedModel{
				Name: "bge-reranker-base", Provider: "ollama",
				Details: map[string]any{"family": "bert"},
			},
			wantRerank: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := deriveCapabilities(tc.model, fakeApps(t))
			if caps.Embeddings != tc.wantEmbeddings {
				t.Errorf("embeddings: got %v, want %v", caps.Embeddings, tc.wantEmbeddings)
			}
			if caps.Chat != tc.wantChat {
				t.Errorf("chat: got %v, want %v", caps.Chat, tc.wantChat)
			}
			if !tc.wantChat && caps.Completions {
				t.Error("a non-chat model must not claim completions")
			}
			if tc.wantRerank != caps.Rerank {
				t.Errorf("rerank: got %v, want %v", caps.Rerank, tc.wantRerank)
			}
		})
	}
}

// An agent picks a model for /v1/messages or /v1/responses off the
// catalog, so each must be advertised exactly when the provider serves
// it, natively or translated, and never on a model with no chat shape.
func TestEndpointsForModel_WireServedEndpoints(t *testing.T) {
	t.Parallel()
	runtime := &pkgConfig.AppRuntimeConfig{
		BasePort:  9000,
		PortRange: []int{9000, 9009},
		Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "x"},
	}
	cfg := &pkgConfig.AppsConfig{}
	for name, eps := range map[string][]string{
		"native": {"chat_completions", "messages", "responses"},
		"compat": {"chat_completions", "messages_compat", "responses_compat"},
		"plain":  {"chat_completions"},
	} {
		sc := pkgConfig.ServiceConfig{Name: name, Protocol: pkgConfig.ProtocolOpenAI, Mode: "on-demand",
			Runtime: runtime, Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: eps}}
		if err := cfg.AddApp(name, sc); err != nil {
			t.Fatalf("AddApp(%q): %v", name, err)
		}
	}
	chat := OpenAIModelCapabilities{Chat: true, Completions: true}
	cases := []struct {
		name     string
		provider string
		caps     OpenAIModelCapabilities
		want     []string
	}{
		{"native", "native", chat, []string{"chat", "completions", "responses", "messages"}},
		{"translated", "compat", chat, []string{"chat", "completions", "responses", "messages"}},
		{"neither", "plain", chat, []string{"chat", "completions"}},
		{"no chat shape", "native", OpenAIModelCapabilities{Embeddings: true}, []string{"embeddings"}},
		{"unknown provider", "gone", chat, []string{"chat", "completions"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := endpointsForModel(tc.caps, &cache.CachedModel{Name: "m", Provider: tc.provider}, cfg)
			if !slices.Equal(got, tc.want) {
				t.Errorf("endpointsForModel = %v, want %v", got, tc.want)
			}
		})
	}
}

// A model's name stands in for vision evidence only for a provider that
// declares no features; a provider that declares them is taken at its word.
func TestDeriveCapabilities_NameGuessOnlyWithoutDeclaredFeatures(t *testing.T) {
	apps := fakeApps(t)
	unknown := map[string]any{"features": map[string]string{"vision": "unknown"}}
	m := &cache.CachedModel{Name: "llava-1.6-7b", Provider: "vllm", Details: unknown}
	if !deriveCapabilities(m, apps).Vision {
		t.Error("undeclared provider: expected the name to stand in for vision")
	}
	if err := apps.UpdateApp("vllm", func(s *pkgConfig.ServiceConfig) error {
		s.Features = map[string]pkgConfig.Feature{"vision": {}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if deriveCapabilities(m, apps).Vision {
		t.Error("declared provider: an unknown state must not become vision from the name")
	}
}
