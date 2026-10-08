package server

// Re-exports of Ollama protocol types from pkg/protocol/ollama.
// The canonical definitions live in pkg/protocol/ollama/types.go;
// this file preserves the Ollama-prefixed names used throughout the server
// layer so the migration is a single-file change.

import (
	"github.com/stperic/zzrouter/pkg/protocol/ollama"
)

type OllamaChatMessage = ollama.ChatMessage
type OllamaInferenceRequest = ollama.InferenceRequest
type OllamaPullRequest = ollama.PullRequest
type OllamaManagementRequest = ollama.ManagementRequest
type OllamaModelDetails = ollama.ModelDetails
type OllamaTagEntry = ollama.TagEntry
type OllamaTagsResponse = ollama.TagsResponse
type OllamaPsEntry = ollama.PsEntry
type OllamaPsResponse = ollama.PsResponse
