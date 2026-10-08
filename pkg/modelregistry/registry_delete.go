package modelregistry

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/filesystem"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/ollama"
)

// GetModel returns a specific model by path.
// For path-based lookups, checks filesystem first (faster than scanning
// all models); falls back to listing all models when path might be a
// model name rather than an absolute path.
func (r *Registry) GetModel(path string) (*metadata.ModelMetadata, error) {
	info, err := os.Stat(path)
	if err == nil {
		md := filesystem.AnalyzeModelFile(r.modelsRoot, path, info)
		if md != nil {
			return md, nil
		}
	}

	var found *metadata.ModelMetadata
	err = r.withModels(func(models []*metadata.ModelMetadata) error {
		for _, m := range models {
			if m.FullPath == path {
				found = m
				return nil
			}
		}
		return fmt.Errorf("model not found: %s", path)
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// AddModel is a no-op since models are discovered via filesystem
// scanning. No cache to invalidate — the server layer handles caching.
func (r *Registry) AddModel(md *metadata.ModelMetadata) error {
	_ = md
	return nil
}

// RemoveModel removes a model from the registry (alias for DeleteModel).
func (r *Registry) RemoveModel(path string) error {
	return r.DeleteModel(path)
}

// ValidateOllamaModel checks if an Ollama model exists in the library.
// Returns (false, nil) when Ollama is not configured — the connector
// treats "not configured" as "model doesn't exist here".
//
// The caller's ctx bounds the Ollama API call; a derived
// WithTimeout(ollama.APITimeout) protects against a stuck upstream.
func (r *Registry) ValidateOllamaModel(ctx context.Context, modelName string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, ollama.APITimeout)
	defer cancel()
	return r.ollamaConnector.ModelExists(cctx, modelName)
}

// ValidateHuggingFaceModel checks if a HuggingFace model exists.
// The caller's ctx bounds the HF API call; a derived
// WithTimeout(constants.HuggingFaceAPITimeout) protects against a
// stuck upstream.
func (r *Registry) ValidateHuggingFaceModel(ctx context.Context, modelID string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, constants.HuggingFaceAPITimeout)
	defer cancel()
	return r.huggingfaceConnector.ModelExists(cctx, modelID)
}

// findModelByNameAndSource finds a model by name and source type (helper
// for deletion). Returns a not-found error if no model matches.
func (r *Registry) findModelByNameAndSource(modelName, source string) (*metadata.ModelMetadata, error) {
	switch strings.ToLower(source) {
	case metadata.SourceOllama:
		// Ollama models are managed by Ollama API, not our registry.
		return nil, fmt.Errorf("ollama models are managed by Ollama API")

	case metadata.SourceHuggingFace:
		models, err := r.huggingfaceConnector.ScanModels()
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			if MatchPattern(m.Name, modelName) {
				return m, nil
			}
		}
		// GGUF files from HuggingFace are stored as individual files,
		// not directories, so they won't be in the HuggingFace
		// connector's ScanModels() results. Check cached models for
		// GGUF files with SourceRepo = HuggingFace.
		var found *metadata.ModelMetadata
		err = r.withModels(func(cachedModels []*metadata.ModelMetadata) error {
			for _, m := range cachedModels {
				if m.Format == metadata.FormatGGUF && m.SourceRepo == metadata.SourceHuggingFace {
					if MatchPattern(m.Name, modelName) {
						found = m
						return nil
					}
				}
			}
			return fmt.Errorf("model not found: %s", modelName)
		})
		if err != nil && found == nil {
			return nil, err
		}
		if found != nil {
			return found, nil
		}

	default:
		// Search in standalone models (GGUF, etc.).
		// For empty source, also check HuggingFace models that are
		// GGUF files (they're stored as files, not directories).
		var found *metadata.ModelMetadata
		err := r.withModels(func(models []*metadata.ModelMetadata) error {
			for _, m := range models {
				if source == "" && m.Format == metadata.FormatGGUF {
					// Include GGUF models regardless of SourceRepo.
					if MatchPattern(m.Name, modelName) {
						found = m
						return nil
					}
				} else if m.SourceRepo != metadata.SourceHuggingFace && m.SourceRepo != metadata.SourceOllama {
					// Non-HuggingFace, non-Ollama models.
					if MatchPattern(m.Name, modelName) {
						found = m
						return nil
					}
				}
			}
			return fmt.Errorf("model not found: %s", modelName)
		})
		if err != nil {
			return nil, err
		}
		if found != nil {
			return found, nil
		}
	}
	return nil, fmt.Errorf("model not found: %s", modelName)
}

// DeleteModelByNameAndSource deletes a model by name and source type.
// The caller's ctx bounds any upstream API call; the Ollama branch
// layers a derived WithTimeout(ollama.APITimeout) on top to protect
// against a stuck upstream.
func (r *Registry) DeleteModelByNameAndSource(ctx context.Context, modelName, source string) error {
	if strings.ToLower(source) == metadata.SourceOllama {
		cctx, cancel := context.WithTimeout(ctx, ollama.APITimeout)
		defer cancel()
		return r.ollamaConnector.DeleteModel(cctx, modelName)
	}

	//nolint:contextcheck // ListAllModels is ctx-less across 25 call sites; buildModelList bridges with its own
	// deadline and says so. Threading ctx through that chain is a refactor, not a lint fix.
	model, err := r.findModelByNameAndSource(modelName, source)
	if err != nil {
		return err
	}

	// For GGUF files (even from HuggingFace), delete the file directly.
	// HuggingFace connector's DeleteModel expects a directory, but GGUF
	// files are individual files.
	if model.Format == metadata.FormatGGUF {
		return r.DeleteModel(model.FullPath)
	}

	// For HuggingFace models that are directories (not GGUF files), use
	// the connector.
	if strings.ToLower(source) == metadata.SourceHuggingFace {
		if err := r.huggingfaceConnector.DeleteModel(model.Name); err != nil {
			return err
		}
		return nil
	}
	// For standalone models, just delete the file.
	return r.DeleteModel(model.FullPath)
}
