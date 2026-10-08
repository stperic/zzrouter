package genai

import (
	"testing"

	"go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"
)

func TestProviderName(t *testing.T) {
	cases := []struct {
		in   string
		want genaiconv.ProviderNameAttr
	}{
		{"openai", genaiconv.ProviderNameOpenAI},
		{"OpenAI", genaiconv.ProviderNameOpenAI},
		{" anthropic ", genaiconv.ProviderNameAnthropic},
		{"azure-openai", genaiconv.ProviderNameAzureAIOpenAI},
		{"azure_openai", genaiconv.ProviderNameAzureAIOpenAI},
		{"azure-inference", genaiconv.ProviderNameAzureAIInference},
		{"bedrock", genaiconv.ProviderNameAWSBedrock},
		{"gemini", genaiconv.ProviderNameGCPGemini},
		{"vertex", genaiconv.ProviderNameGCPVertexAI},
		{"vertex_ai", genaiconv.ProviderNameGCPVertexAI},
		{"mistral", genaiconv.ProviderNameMistralAI},
		{"xai", genaiconv.ProviderNameXAI},
		{"watsonx", genaiconv.ProviderNameIBMWatsonxAI},
		{"vllm", "vllm"},
		{"llama.cpp", "llama_cpp"},
		{"llama_cpp", "llama_cpp"},
		{"mlx", "mlx"},
		{"ollama", "ollama"},
		// Unknown keys pass through normalised.
		{"WeirdNew", "weirdnew"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ProviderName(c.in); got != c.want {
			t.Errorf("ProviderName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOperationName(t *testing.T) {
	cases := []struct {
		in   string
		want genaiconv.OperationNameAttr
	}{
		{"/v1/chat/completions", OperationChat},
		{"/v1/chat/completions?stream=true", OperationChat},
		{"/v1/chat/completions/", OperationChat},
		{"/v1/completions", OperationTextCompletion},
		{"/v1/embeddings", OperationEmbeddings},
		{"/v1/responses", OperationChat},
		{"/v1/moderations", OperationModeration},
		{"/v1/audio/speech", OperationAudioSpeech},
		{"/v1/audio/transcriptions", OperationAudioTranscription},
		{"/v1/audio/translations", OperationAudioTranslation},
		{"/v1/images/generations", OperationImageGeneration},
		{"/v1/images/edits", OperationImageEdit},
		{"/v1/images/variations", OperationImageVariation},
		{"/v1/rerank", OperationRerank},
		{"/v1/realtime", OperationRealtimeSession},
		{"/v1/vector_stores/vs_abc/search", OperationRetrieval},
		{"/v1/vector_stores/vs_xyz/search?limit=10", OperationRetrieval},
		// Ollama-native /api/* — closed-enum mapping shared with
		// the /v1/* counterparts so dashboards filter cleanly across
		// both protocol surfaces. /api/generate maps to OperationChat
		// (matching RequestTypeGenerate in the recorder mapping at
		// pkg/observability/llm/metrics.go::RequestTypeToOperation).
		{"/api/chat", OperationChat},
		{"/api/generate", OperationChat},
		{"/api/embed", OperationEmbeddings},
		{"/api/embeddings", OperationEmbeddings},
		// Unknown routes → empty.
		{"/v1/unknown", ""},
		{"/api/unknown", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := OperationName(c.in); got != c.want {
			t.Errorf("OperationName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
