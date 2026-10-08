// Package metadata holds the DTOs and constants shared by
// pkg/modelregistry and its source sub-packages. This is a leaf
// package: it must not import pkg/modelregistry or any source
// sub-package, so that pkg/modelregistry/source/{ollama,huggingface,
// filesystem} can depend on these types without creating an import
// cycle through the orchestrator.
package metadata

import (
	"strings"
	"time"
)

// ModelMetadata represents metadata about a downloaded model file.
// Internal format — converted to Ollama format at the API boundary via
// ToOllamaFormat.
type ModelMetadata struct {
	Name         string    `json:"name"`                   // Display name (e.g., "llama2-7b-chat")
	FullPath     string    `json:"full_path"`              // Complete filesystem path
	Format       string    `json:"format"`                 // GGUF, safetensors, hf_transformers, etc.
	Quantization string    `json:"quantization,omitempty"` // Q4_K_M, Q8_0, fp16, bf16, etc.
	Size         int64     `json:"size"`                   // File/directory size in bytes
	SourceRepo   string    `json:"source_repo"`            // huggingface, ollama, etc.
	SourceID     string    `json:"source_id"`              // TheBloke/Llama-2-7B-Chat-GGUF or Qwen/Qwen2.5-VL
	Modified     time.Time `json:"modified"`               // Last modified time
	Digest       string    `json:"digest,omitempty"`       // Model digest/hash (from Ollama)
	AddedAt      time.Time `json:"added_at"`               // When added to index
	Node         string    `json:"node,omitempty"`         // Node where model resides (for cluster aggregation)

	// Provider Assignment (for routing)
	AssignedApp      string            `json:"assigned_app,omitempty"`      // Provider key (e.g., "llamacpp", "ollama") - DEPRECATED: Use Assignments
	AssignedNode     string            `json:"assigned_host,omitempty"`     // Node name (e.g., "localhost", "gpu-server") - DEPRECATED
	AssignmentSource string            `json:"assignment_source,omitempty"` // "auto", "explicit", "config" - DEPRECATED
	AssignedAt       time.Time         `json:"assigned_at"`                 // When assignment was made - DEPRECATED
	Assignments      map[string]string `json:"assignments,omitempty"`       // Node-specific provider assignments (host → provider)

	// Optional metadata from download time
	DownloadedFrom string         `json:"downloaded_from,omitempty"` // Original URL
	DownloadDate   time.Time      `json:"download_date"`             // When downloaded
	Extra          map[string]any `json:"extra,omitempty"`           // Additional metadata (includes Ollama details)
}

// ToOllamaFormat converts ModelMetadata to API response format.
// Despite the name, this works for ALL model types (Ollama, vLLM,
// HuggingFace, MLX, etc.). The name is historical — it returns a
// standardized response format compatible with the Ollama API
// structure.
func (m *ModelMetadata) ToOllamaFormat(host string) map[string]any {
	// Use the model's Node field if set (cluster aggregation),
	// otherwise use the provided host.
	modelNode := m.Node
	if modelNode == "" {
		modelNode = host
	}

	response := map[string]any{
		"name":         m.Name,
		"model":        m.Name, // Ollama duplicates name in model field
		"modified_at":  m.Modified,
		"size":         m.Size,
		"digest":       m.Digest,
		"node":         modelNode,
		"source_repo":  m.SourceRepo,  // Where it came from (ollama, huggingface)
		"source_id":    m.SourceID,    // Original ID
		"assigned_app": m.AssignedApp, // What will run it (mlx, test, etc.)
	}

	if m.Extra != nil {
		if details, ok := m.Extra["details"].(map[string]any); ok {
			// Ensure format is always set from ModelMetadata.Format
			// (the source of truth).
			details["format"] = m.Format
			if m.Quantization != "" {
				details["quantization_level"] = m.Quantization
			}
			response["details"] = details
		} else {
			// Build details from our fields for non-Ollama models.
			response["details"] = map[string]any{
				"format":             m.Format,
				"quantization_level": m.Quantization,
			}
		}
	} else {
		// Build basic details from our fields.
		response["details"] = map[string]any{
			"format":             m.Format,
			"quantization_level": m.Quantization,
		}
	}

	return response
}

// Format constants — aligned with pkg/models/models.go.
const (
	FormatGGUF        = "gguf"
	FormatSafetensors = "safetensors"
	FormatHuggingFace = "hf_transformers"
	FormatMLX         = "mlx"
	FormatPyTorch     = "pytorch"
	FormatONNX        = "onnx"
	FormatTensorRTLLM = "tensorrt_llm"
)

// Source repository constants.
const (
	SourceHuggingFace = "huggingface"
	SourceOllama      = "ollama"
	SourceLocal       = "local"
	SourceUnknown     = "unknown"
)

// RepoAppCompatible reports whether an app type can serve models from a
// given source repository. Ollama is a walled garden: Ollama-sourced
// models require Ollama apps, and Ollama apps refuse non-Ollama models.
// All other repo/app combinations are open.
//
// This is the single authoritative place the "ollama is exclusive" rule
// is expressed — callers (routing, search filters, auto-assignment)
// should call this helper rather than hand-rolling the comparison.
//
// TODO(multi-walled-garden): if a second exclusive source ever ships,
// the static check below turns into N branches. Replace with a per-
// source compatibility callback (e.g. a `sourceRules` map keyed by
// source name) at that point — premature now, but keep this TODO so the
// smell doesn't silently compound.
func RepoAppCompatible(repo, appType string) bool {
	if repo == "" {
		return true
	}
	ollamaRepo := strings.EqualFold(repo, SourceOllama)
	ollamaApp := strings.EqualFold(appType, SourceOllama)
	return ollamaRepo == ollamaApp
}
