// Package adapter defines the protocol-adapter contract used by zzRouter's
// compat surfaces (OpenAI /v1/*, Ollama /api/*, and any future protocol).
//
// Protocol adapters translate canonical ModelService types
// (cache.CachedModel, ShowModelResponse, RunningInstance) into protocol-
// specific wire-format shapes. A new protocol surface is added by writing
// one adapter + registering its routes — no changes to ModelService, the
// cluster layer, the cache, or routing.
//
// The canonical layer stays protocol-neutral. Handlers orchestrate:
// parse request → call ModelService → translate via adapter → serialize.
package adapter

import "github.com/stperic/zzrouter/pkg/model/cache"

// ProtocolAdapter shapes canonical model data into a protocol's wire format.
// All methods return an `any` that must be JSON-marshallable by the Gin
// encoder; the concrete type is protocol-specific.
type ProtocolAdapter interface {
	// ListEntry translates one canonical model into this protocol's list-
	// entry shape. Called once per model in a list response.
	ListEntry(m *cache.CachedModel) any

	// ListEnvelope wraps a slice of already-translated entries in the
	// protocol's list-response envelope.
	// OpenAI: {"object":"list","data":[...]}
	// Ollama: {"models":[...]}
	ListEnvelope(entries []any) any

	// ShowResponse translates ShowModelResponse into the protocol's
	// show-model shape.
	ShowResponse(r *ShowModelResponse) any
}

// OptionalRunningModelAdapter is implemented by protocols that expose a
// runtime introspection endpoint (e.g., Ollama's /api/ps). Handlers test
// for it via type assertion; protocols without the concept simply do not
// implement it.
type OptionalRunningModelAdapter interface {
	// RunningEntry translates one running-instance record into the
	// protocol's ps-entry shape.
	RunningEntry(r *RunningInstance) any
}

// RunningInstance describes a model instance currently loaded in provider
// memory (VRAM for GPU-backed providers). It is the canonical shape
// protocol adapters receive; it must NOT be confused with cache.CachedModel,
// which is a static-catalog entry.
//
// Name-field convention — uniform across ALL providers (Ollama, vLLM,
// MLX, llama.cpp, cloud-backed, whatever comes next):
//
//	Name  = "<Model>@<Node>"   — addressable identifier for this specific
//	                             instance. Matches the canonical name
//	                             grammar: artifact on the left, qualifier
//	                             on the right. Parseable via
//	                             modelregistry.Parse.
//	Model = "<Model>"           — logical model name, bare. Multiple
//	                             RunningInstance values may share a Model
//	                             when replicas exist on different nodes;
//	                             clients group by this field when they
//	                             want the "route view."
//	Node  = "<Node>"            — source node (also embedded in Name).
//
// Adapters translate this canonical form into protocol-specific wire
// shapes (e.g., OllamaPsEntry) without re-deriving the grammar.
type RunningInstance struct {
	Name          string // "<Model>@<Node>" — canonical addressable form
	Model         string // "<Model>"        — bare logical name
	Node          string // "<Node>"         — source node
	Provider      string
	SizeVRAM      int64
	ContextLength int
	ExpiresAt     int64 // Unix seconds; 0 = no timeout
	Digest        string
}
