package filesystem

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// IsModelFile reports whether a file path has a recognized model-file
// extension. Excludes files in known variant subdirectories (onnx/,
// openvino/, etc.) — those are alternate formats of a model, not
// separate models.
func IsModelFile(path string) bool {
	pathParts := strings.SplitSeq(filepath.ToSlash(path), "/")
	for part := range pathParts {
		if part == "onnx" || part == "openvino" || part == "coreml" || part == "tflite" {
			return false
		}
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".gguf":
		return true
	case ".safetensors":
		return true
	case ".bin": // PyTorch models
		return true
	case ".onnx":
		return true
	case ".engine": // TensorRT
		return true
	case ".llm": // LLM models
		return true
	default:
		return false
	}
}

// AnalyzeModelFile inspects a single model file and returns its
// ModelMetadata. modelsRoot is the absolute path to the on-disk models
// root — used to extract HuggingFace-style "org/name" identifiers from
// full paths. Returns nil for files that should not be indexed
// individually.
func AnalyzeModelFile(modelsRoot, path string, info os.FileInfo) *metadata.ModelMetadata {
	ext := strings.ToLower(filepath.Ext(path))
	basename := filepath.Base(path)

	md := &metadata.ModelMetadata{
		Name:     strings.TrimSuffix(basename, ext),
		FullPath: path,
		Size:     info.Size(),
		Modified: info.ModTime(),
		AddedAt:  info.ModTime(), // Use file mtime as default
	}

	switch ext {
	case ".gguf":
		analyzeGGUF(modelsRoot, md, path, basename)
	case ".safetensors":
		analyzeSafetensors(modelsRoot, md, path)
	case ".bin":
		analyzePyTorch(modelsRoot, md, path)
	case ".onnx":
		analyzeONNX(md)
	case ".engine":
		analyzeTensorRT(md)
	}

	inferSourceRegistry(modelsRoot, md, path)

	return md
}

// analyzeGGUF inspects a GGUF file. When the file lives inside a
// HuggingFace-style "<org>/<repo>/" directory, the repo path is
// recorded as SourceID so /v1/* clients can address the model by the
// repo name they used at deploy/launch (e.g. Qwen/Qwen2.5-0.5B-Instruct-GGUF)
// in addition to the file-stem alias (e.g. qwen2.5-0.5b-instruct-q4_k_m).
func analyzeGGUF(modelsRoot string, md *metadata.ModelMetadata, path, basename string) {
	md.Format = metadata.FormatGGUF
	if quant := metadata.ExtractQuantization(basename); quant != "" {
		md.Quantization = quant
	}
	if modelID := extractModelIDFromPath(modelsRoot, path); modelID != "" {
		md.SourceID = modelID
	}
}

// analyzeSafetensors inspects a safetensors file.
func analyzeSafetensors(modelsRoot string, md *metadata.ModelMetadata, path string) {
	md.Format = metadata.FormatSafetensors

	dir := filepath.Dir(path)
	if hasConfigJSON(dir) {
		md.SourceRepo = metadata.SourceHuggingFace

		if modelID := extractModelIDFromPath(modelsRoot, path); modelID != "" {
			md.SourceID = modelID
			md.Name = modelID // Use full model ID as name (e.g., "Qwen/Qwen3-0.6B")
		}

		md.Format = detectFormatFromDirectory(dir, md.Name)
	}
}

// analyzePyTorch inspects a PyTorch .bin file.
func analyzePyTorch(modelsRoot string, md *metadata.ModelMetadata, path string) {
	md.Format = metadata.FormatPyTorch

	dir := filepath.Dir(path)
	if hasConfigJSON(dir) {
		md.Format = metadata.FormatHuggingFace
		md.SourceRepo = metadata.SourceHuggingFace

		if modelID := extractModelIDFromPath(modelsRoot, path); modelID != "" {
			md.SourceID = modelID
			md.Name = modelID
		}
	}
}

// analyzeONNX inspects an ONNX file.
func analyzeONNX(md *metadata.ModelMetadata) {
	md.Format = metadata.FormatONNX
}

// analyzeTensorRT inspects a TensorRT engine file.
func analyzeTensorRT(md *metadata.ModelMetadata) {
	md.Format = metadata.FormatTensorRTLLM
}

// detectFormatFromDirectory detects the specific format of a
// HuggingFace model directory in order of specificity:
//  1. Model-name patterns (-MLX, -GGUF, quantization indicators)
//  2. File extensions (.npz for MLX, .gguf for GGUF)
//  3. Special files (mlx_model.safetensors)
//  4. Default to hf_transformers
func detectFormatFromDirectory(modelDir, modelName string) string {
	upper := strings.ToUpper(modelName)
	if strings.Contains(upper, "-MLX") || strings.Contains(upper, "MLX-") || strings.Contains(upper, "/MLX") {
		return metadata.FormatMLX
	}
	if strings.Contains(upper, "-GGUF") || strings.Contains(upper, "GGUF-") {
		return metadata.FormatGGUF
	}

	hasMLXFiles := false
	hasGGUFFiles := false

	_ = filepath.Walk(modelDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // keep walking; per-entry errors are surfaced via the aggregate result
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

// hasConfigJSON reports whether dir contains a HuggingFace config.json.
func hasConfigJSON(dir string) bool {
	configPath := filepath.Join(dir, "config.json")
	_, err := os.Stat(configPath)
	return err == nil
}

// hasGitAttributes reports whether dir contains a .gitattributes file.
func hasGitAttributes(dir string) bool {
	gitAttrPath := filepath.Join(dir, ".gitattributes")
	_, err := os.Stat(gitAttrPath)
	return err == nil
}

// extractModelIDFromPath extracts a HuggingFace-style "org/name" model
// ID from a full file path relative to modelsRoot. Example:
// /models/Qwen/Qwen2.5-VL-7B/model.safetensors → Qwen/Qwen2.5-VL-7B.
func extractModelIDFromPath(modelsRoot, path string) string {
	dir := filepath.Dir(path)

	relPath, err := filepath.Rel(modelsRoot, dir)
	if err != nil {
		return ""
	}

	relPath = filepath.ToSlash(relPath)

	parts := strings.Split(relPath, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[:2], "/")
	}

	return ""
}

// inferSourceRegistry tries to infer the source repository from the
// file path when analyzeXxx did not already set one.
func inferSourceRegistry(modelsRoot string, md *metadata.ModelMetadata, path string) {
	if md.SourceRepo != "" {
		return
	}

	lowerPath := strings.ToLower(path)

	if strings.Contains(lowerPath, "huggingface") || strings.Contains(lowerPath, "hf") {
		md.SourceRepo = metadata.SourceHuggingFace
	} else if strings.Contains(lowerPath, "ollama") {
		md.SourceRepo = metadata.SourceOllama
	} else {
		// Check if path structure suggests HuggingFace model (org/model
		// pattern). Catches GGUF models downloaded from HuggingFace
		// that might not have a config.json.
		if modelID := extractModelIDFromPath(modelsRoot, path); modelID != "" {
			md.SourceRepo = metadata.SourceHuggingFace
			md.SourceID = modelID
		} else {
			md.SourceRepo = metadata.SourceUnknown
		}
	}
}
