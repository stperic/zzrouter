package huggingface

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// ModelExists checks if a model exists on HuggingFace.
// Returns true if the model exists, false for 404/401.
func (hfc *Connector) ModelExists(ctx context.Context, modelID string) (bool, error) {
	apiURL := fmt.Sprintf("%s%s", hfc.apiBase, modelID)

	req, err := hfc.newGet(ctx, apiURL)
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound, http.StatusUnauthorized:
		return false, nil
	}
	return false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
}

// ScanModels scans the models directory for downloaded HuggingFace
// models. HuggingFace models are identified by having config.json or
// .gitattributes files. Variant subdirectories (onnx/, openvino/, etc.)
// are skipped to avoid duplicates. Returns one ModelMetadata per model
// directory (not per file) — except for GGUF repos, where one entry is
// created per .gguf variant so users can pick a quantization.
func (hfc *Connector) ScanModels() ([]*metadata.ModelMetadata, error) {
	var models []*metadata.ModelMetadata

	if _, err := os.Stat(hfc.modelsDir); os.IsNotExist(err) {
		return models, nil
	}

	addedModels := make(map[string]bool)

	err := filepath.Walk(hfc.modelsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		basename := filepath.Base(path)

		// HuggingFace marker files live in the model root.
		if basename != "config.json" && basename != ".gitattributes" {
			return nil
		}

		modelDir := filepath.Dir(path)

		// Skip files inside known variant subdirectories.
		pathParts := strings.SplitSeq(filepath.ToSlash(path), "/")
		for part := range pathParts {
			if part == "onnx" || part == "openvino" || part == "coreml" || part == "tflite" {
				return nil
			}
		}

		// Mark as added immediately — prevents duplicate entry when both
		// config.json and .gitattributes land the walker in the same dir.
		if addedModels[modelDir] {
			return nil
		}
		addedModels[modelDir] = true

		// Extract model name from path relative to models root (e.g. "Qwen/Qwen3-0.6B").
		relPath, _ := filepath.Rel(hfc.modelsDir, modelDir)
		modelName := strings.ReplaceAll(relPath, string(filepath.Separator), "/")

		// Ollama models typically lack the org/name slash — skip them here.
		if !strings.Contains(modelName, "/") {
			return nil
		}

		dirInfo, err := os.Stat(modelDir)
		if err != nil {
			return nil
		}

		totalSize := int64(0)
		_ = filepath.Walk(modelDir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				totalSize += fi.Size()
			}
			return nil
		})

		configPath := filepath.Join(modelDir, "config.json")
		extra := parseHuggingFaceConfig(configPath)
		format := detectModelFormat(modelDir, modelName)

		// GGUF repos: one entry per weights variant; a split set is one
		// entry, and a feature's file is none.
		if format == metadata.FormatGGUF {
			files, _ := layout.OnDisk(modelDir)
			if variants := layout.GGUFVariants(files); len(variants) > 0 {
				for _, v := range variants {
					models = append(models, &metadata.ModelMetadata{
						Name:         v.Name,
						FullPath:     filepath.Join(modelDir, filepath.FromSlash(v.Files[0].Path)),
						Format:       format,
						Quantization: metadata.ExtractQuantization(v.Files[0].Path),
						Size:         v.Size(),
						SourceRepo:   metadata.SourceHuggingFace,
						SourceID:     modelName,
						Modified:     dirInfo.ModTime(),
						AddedAt:      dirInfo.ModTime(),
						Extra:        extra,
					})
				}
				return filepath.SkipDir
			}
		}

		models = append(models, &metadata.ModelMetadata{
			Name:       modelName,
			FullPath:   modelDir,
			Format:     format,
			Size:       totalSize,
			SourceRepo: metadata.SourceHuggingFace,
			SourceID:   modelName,
			Modified:   dirInfo.ModTime(),
			AddedAt:    dirInfo.ModTime(),
			Extra:      extra,
		})

		return filepath.SkipDir
	})

	_ = err
	return models, nil
}

// detectModelFormat returns the concrete format of a HuggingFace model
// directory. Checks in order of specificity: (1) format hints in the
// model name (e.g. -MLX, -GGUF), (2) marker files (.npz → MLX, .gguf →
// GGUF), (3) defaults to the generic transformers format.
func detectModelFormat(modelDir, modelName string) string {
	upper := strings.ToUpper(modelName)
	if strings.Contains(upper, "-MLX") || strings.Contains(upper, "MLX-") {
		return metadata.FormatMLX
	}
	if strings.Contains(upper, "-GGUF") || strings.Contains(upper, "GGUF-") {
		return metadata.FormatGGUF
	}

	hasMLXFiles := false
	hasGGUFFiles := false

	_ = filepath.Walk(modelDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // best-effort scan: keep walking past per-entry errors
		}
		basename := filepath.Base(path)
		ext := strings.ToLower(filepath.Ext(basename))

		if ext == ".npz" || basename == "mlx_model.safetensors" {
			hasMLXFiles = true
			return filepath.SkipDir
		}
		if ext == ".gguf" {
			hasGGUFFiles = true
			return filepath.SkipDir
		}
		return nil
	})

	if hasMLXFiles {
		return metadata.FormatMLX
	}
	if hasGGUFFiles {
		return metadata.FormatGGUF
	}
	return metadata.FormatHuggingFace
}

// parseHuggingFaceConfig parses config.json to extract Ollama-compatible
// metadata. Returns an empty map if the file is missing or unparseable.
func parseHuggingFaceConfig(configPath string) map[string]any {
	extra := make(map[string]any)

	data, err := os.ReadFile(configPath)
	if err != nil {
		return extra
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return extra
	}

	details := make(map[string]any)
	if vision, exists := config["vision_config"]; exists {
		details["vision"] = vision != nil
	}

	// model_type → family (e.g. "llama", "qwen2", "mistral").
	if modelType, ok := config["model_type"].(string); ok {
		details["family"] = modelType
		details["families"] = []string{modelType}
	} else if architectures, ok := config["architectures"].([]any); ok && len(architectures) > 0 {
		if arch, ok := architectures[0].(string); ok {
			family := extractFamilyFromArchitecture(arch)
			details["family"] = family
			details["families"] = []string{family}
		}
	}

	// torch_dtype → quantization_level (e.g. "bfloat16", "float16", "int8").
	if torchDtype, ok := config["torch_dtype"].(string); ok {
		details["quantization_level"] = torchDtype
	}

	// Rough parameter size estimate from (hidden_size * num_hidden_layers * vocab_size * 2) / 1e9.
	if hiddenSize, ok := config["hidden_size"].(float64); ok {
		if numLayers, ok := config["num_hidden_layers"].(float64); ok {
			if vocabSize, ok := config["vocab_size"].(float64); ok {
				params := (hiddenSize * numLayers * vocabSize * 2) / 1e9
				details["parameter_size"] = fmt.Sprintf("%.1fB", params)
			}
		}
	}

	if parentModel, ok := config["_name_or_path"].(string); ok {
		details["parent_model"] = parentModel
	} else {
		details["parent_model"] = ""
	}

	extra["details"] = details
	return extra
}

// extractFamilyFromArchitecture extracts the model family from an
// architecture name, e.g. "LlamaForCausalLM" → "llama",
// "Qwen2ForCausalLM" → "qwen2".
func extractFamilyFromArchitecture(arch string) string {
	arch = strings.TrimSuffix(arch, "ForCausalLM")
	arch = strings.TrimSuffix(arch, "ForConditionalGeneration")
	arch = strings.TrimSuffix(arch, "Model")
	return strings.ToLower(arch)
}
