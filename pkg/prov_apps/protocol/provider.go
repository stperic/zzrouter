package protocol

import (
	"context"
	"time"
)

// Provider is the base interface for all provider implementations.
type Provider interface {
	Name() string
	Type() string
	DefaultPort() int
}

// ModelLister can list available models.
type ModelLister interface {
	Provider
	ListModels(ctx context.Context, t Target) ([]ModelInfo, error)
}

// ModelLoader can load and unload models.
type ModelLoader interface {
	Provider
	// LoadModel loads model for the daemon to expire after keepAlive idle (never when negative, its default when nil).
	LoadModel(ctx context.Context, t Target, model string, keepAlive *time.Duration) error
	UnloadModel(ctx context.Context, t Target, model string) error
	ListRunningModels(ctx context.Context, t Target) ([]RunningModelInfo, error)
	// Identity is the name the daemon reports model loaded under.
	Identity(model string) string
}

// ModelDeleter can delete models from disk/storage.
type ModelDeleter interface {
	Provider
	DeleteModel(ctx context.Context, t Target, model string) error
}

// ModelInspector can show detailed info about an app or model.
type ModelInspector interface {
	Provider
	ShowApp(ctx context.Context, t Target) (map[string]any, error)
	ShowModel(ctx context.Context, t Target, model string) (map[string]any, error)
}

// ModelDisplayer can render a table of models to stdout.
type ModelDisplayer interface {
	Provider
	DisplayTable(models []DisplayModel, command string)
}

// FullProvider provides catalog and inspection operations for every protocol.
type FullProvider interface {
	ModelLister
	ModelDeleter
	ModelInspector
	ModelDisplayer
}

// --- Data types ---

// ModelInfo represents basic model information.
//
// Modified is the display-formatted ("2006-01-02 15:04") string used by the
// table-rendering CLI paths. ModifiedAt carries the underlying time.Time for
// callers that need to re-serialize it (e.g. the Ollama compat surface, which
// emits RFC3339). Providers should populate both.
type ModelInfo struct {
	Name       string
	FullID     string
	Size       int64
	Modified   string
	ModifiedAt time.Time
	Digest     string
	Extra      map[string]any
}

// RunningModelInfo represents a running model.
type RunningModelInfo struct {
	Name          string
	Digest        string
	ExpiresAt     time.Time
	ContextLength int
	Size          int64
	SizeVRAM      int64
	Format        string
	Family        string
	ParameterSize string
	QuantLevel    string
}

// DisplayModel represents model data for table display.
type DisplayModel struct {
	Name          string
	Status        string
	MemoryMB      int64
	Size          int64
	Modified      string
	Digest        string
	ExpiresAt     string
	ContextLength int
	SizeVRAM      int64
	Format        string
	Family        string
	ParameterSize string
	QuantLevel    string
}
