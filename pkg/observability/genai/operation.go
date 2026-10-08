package genai

import (
	"strings"

	"go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"
)

// Operation values used for `gen_ai.operation.name`. OTel-canonical values
// come from genaiconv; custom values are namespaced (`audio.transcription`,
// `image.generation`, ...) so that future OTel additions can replace them
// without churning labels elsewhere in the codebase.
var (
	// OTel canonical operations.
	OperationChat           = genaiconv.OperationNameChat
	OperationTextCompletion = genaiconv.OperationNameTextCompletion
	OperationEmbeddings     = genaiconv.OperationNameEmbeddings
	OperationRetrieval      = genaiconv.OperationNameRetrieval
	OperationExecuteTool    = genaiconv.OperationNameExecuteTool

	// Custom (zzrouter-defined) operations, namespaced to avoid future
	// collisions if OTel canonicalises any of them.
	OperationModeration         genaiconv.OperationNameAttr = "moderation"
	OperationAudioSpeech        genaiconv.OperationNameAttr = "audio.speech"
	OperationAudioTranscription genaiconv.OperationNameAttr = "audio.transcription"
	OperationAudioTranslation   genaiconv.OperationNameAttr = "audio.translation"
	OperationImageGeneration    genaiconv.OperationNameAttr = "image.generation"
	OperationImageEdit          genaiconv.OperationNameAttr = "image.edit"
	OperationImageVariation     genaiconv.OperationNameAttr = "image.variation"
	OperationRerank             genaiconv.OperationNameAttr = "rerank"
	OperationRealtimeSession    genaiconv.OperationNameAttr = "realtime.session"
)

// OperationName maps a route path (e.g. "/v1/chat/completions") onto its
// `gen_ai.operation.name` value. The query string and any leading/trailing
// whitespace are ignored. An unknown route returns an empty string so the
// caller can decide whether to omit the attribute or substitute a fallback.
func OperationName(route string) genaiconv.OperationNameAttr {
	r := strings.TrimSpace(route)
	if i := strings.IndexByte(r, '?'); i >= 0 {
		r = r[:i]
	}
	r = strings.TrimRight(r, "/")
	if v, ok := operationNames[r]; ok {
		return v
	}
	// Vector store search routes — `/v1/vector_stores/{id}/search`.
	if strings.HasPrefix(r, "/v1/vector_stores/") && strings.HasSuffix(r, "/search") {
		return OperationRetrieval
	}
	return ""
}

var operationNames = map[string]genaiconv.OperationNameAttr{
	"/v1/chat/completions":     OperationChat,
	"/v1/completions":          OperationTextCompletion,
	"/v1/embeddings":           OperationEmbeddings,
	"/v1/responses":            OperationChat,
	"/v1/messages":             OperationChat,
	"/v1/moderations":          OperationModeration,
	"/v1/audio/speech":         OperationAudioSpeech,
	"/v1/audio/transcriptions": OperationAudioTranscription,
	"/v1/audio/translations":   OperationAudioTranslation,
	"/v1/images/generations":   OperationImageGeneration,
	"/v1/images/edits":         OperationImageEdit,
	"/v1/images/variations":    OperationImageVariation,
	"/v1/rerank":               OperationRerank,
	"/v1/realtime":             OperationRealtimeSession,

	// Ollama-native /api/* — same closed-enum operation values as
	// the OpenAI counterparts so a single dashboard filter on
	// gen_ai.operation.name covers both protocol surfaces.
	// /api/generate maps to OperationChat (NOT text_completion) to
	// match RequestTypeToOperation's RequestTypeGenerate -> OperationChat
	// at pkg/observability/llm/metrics.go::RequestTypeToOperation. Ollama's
	// /api/generate accepts conversational system+context fields so the
	// chat shape is closer to its actual semantic. Mismatch between the
	// route map and the recorder map would corrupt dashboards joining
	// on operation.name across inflight + inference series.
	"/api/chat":       OperationChat,
	"/api/generate":   OperationChat,
	"/api/embed":      OperationEmbeddings,
	"/api/embeddings": OperationEmbeddings,
}
