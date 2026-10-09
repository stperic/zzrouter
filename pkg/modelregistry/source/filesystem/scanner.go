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
	"sort"

	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// ScanModels walks modelsDir and returns a ModelMetadata entry per
// recognized standalone model: one per file, except GGUF, where a
// directory's files group into weights variants (a split set is one
// entry, an incomplete one none, and a file the manifest assigns to a
// feature none). HuggingFace model directories are
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
	ggufDirs := make(map[string][]string)

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

		if layout.IsGGUF(path) {
			ggufDirs[parentDir] = append(ggufDirs[parentDir], path)
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // best-effort scan: skip entries whose info is unreadable
		}
		md := AnalyzeModelFile(modelsDir, path, info)
		complete, err := layout.Complete(path)
		md.Incomplete = !complete || err != nil
		models = appendModel(models, md)
		addedFiles[path] = true
		return nil
	})

	dirs := make([]string, 0, len(ggufDirs))
	for dir := range ggufDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		models = append(models, ggufVariants(modelsDir, dir, ggufDirs[dir])...)
	}
	return models, err
}

// ggufVariants lists the GGUF weights variants whose files sit directly in
// dir, grouped the way downloads and launches group them. When the
// directory cannot be listed, each of its GGUF paths stands alone.
func ggufVariants(modelsDir, dir string, paths []string) []*metadata.ModelMetadata {
	var models []*metadata.ModelMetadata
	files, err := layout.InDir(dir)
	if err != nil {
		for _, path := range paths {
			if info, err := os.Stat(path); err == nil {
				models = appendModel(models, AnalyzeModelFile(modelsDir, path, info))
			}
		}
		return models
	}
	for _, v := range layout.GGUFVariants(files) {
		path := filepath.Join(dir, v.Files[0].Path)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		md := AnalyzeModelFile(modelsDir, path, info)
		md.Name = v.Name
		md.Size = v.Size()
		models = appendModel(models, md)
	}
	return models
}

// appendModel keeps md unless Ollama owns it (the daemon lists those).
func appendModel(models []*metadata.ModelMetadata, md *metadata.ModelMetadata) []*metadata.ModelMetadata {
	if md == nil || md.SourceRepo == metadata.SourceOllama {
		return models
	}
	return append(models, md)
}
