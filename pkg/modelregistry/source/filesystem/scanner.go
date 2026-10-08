// Package filesystem scans an on-disk models root for standalone
// GGUF/safetensors/etc model files that are not part of a HuggingFace
// model directory. This is the third source under
// pkg/modelregistry/source (alongside ollama and huggingface).
//
// Invariant: this package must not import pkg/modelregistry. The
// orchestrator depends on us, not the other way around — see
// pkg/modelregistry/source/internal_import_guard_test.go.
package filesystem

import (
	"os"
	"path/filepath"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// ScanModels walks modelsDir and returns a ModelMetadata entry per
// recognized standalone model file. HuggingFace model directories are
// skipped (they are handled by the HuggingFace connector); Ollama
// models are skipped too (the Ollama daemon owns those). Errors are
// ignored mid-walk — a single unreadable file must not drop the whole
// scan.
func ScanModels(modelsDir string) ([]*metadata.ModelMetadata, error) {
	var models []*metadata.ModelMetadata

	if _, err := os.Stat(modelsDir); os.IsNotExist(err) {
		return models, nil // No models directory yet
	}

	addedFiles := make(map[string]bool)

	err := filepath.WalkDir(modelsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort scan: keep walking past per-entry errors
		}

		if d.IsDir() {
			return nil
		}

		if !IsModelFile(path) {
			return nil
		}

		if addedFiles[path] {
			return nil
		}

		// Skip HuggingFace model directories — they are the HF
		// scanner's territory.
		parentDir := filepath.Dir(path)
		if hasConfigJSON(parentDir) || hasGitAttributes(parentDir) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // best-effort scan: skip entries whose info is unreadable
		}

		md := AnalyzeModelFile(modelsDir, path, info)
		if md != nil {
			// Skip Ollama models (managed by Ollama API).
			if md.SourceRepo != metadata.SourceOllama {
				models = append(models, md)
				addedFiles[path] = true
			}
		}

		return nil
	})

	return models, err
}
