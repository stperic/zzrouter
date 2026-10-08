package modelregistry

import (
	"fmt"
	"sort"
	"strings"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// ============================================================================
// Shared Format Detection Utilities
// ============================================================================
//
// These exported functions are the single source of truth for format detection
// from model names. Used by: server compatibility checks, CLI deploy flow, TUI
// node selection. Keep all format-from-name logic here — do not duplicate.
//
// Filesystem-specific analysis helpers (IsModelFile, AnalyzeModelFile,
// hasConfigJSON, etc.) live in pkg/modelregistry/source/filesystem;
// quant extraction lives in pkg/modelregistry/metadata.

// DetectFormatFromName detects a model format from its name alone.
// Returns a Format* constant (e.g. metadata.FormatGGUF) or "" if the format cannot
// be determined from the name. When "" is returned, callers should use
// provider config capabilities.formats to determine compatibility rather than
// assuming a default format.
func DetectFormatFromName(modelName string) string {
	lower := strings.ToLower(modelName)
	switch {
	case strings.Contains(lower, "gguf"):
		return metadata.FormatGGUF
	case strings.Contains(lower, "mlx"):
		return metadata.FormatMLX
	case strings.Contains(lower, "safetensors"):
		return metadata.FormatSafetensors
	}
	return ""
}

// DetectFormatFromTags detects a model format from its tags (e.g. HuggingFace tags).
// Checks for known format tags: gguf, mlx, safetensors.
// For HuggingFace models with "transformers" tag (but no specific format), returns metadata.FormatHuggingFace.
func DetectFormatFromTags(tags []string) string {
	hasTransformers := false
	for _, tag := range tags {
		lower := strings.ToLower(tag)
		switch lower {
		case "gguf":
			return metadata.FormatGGUF
		case "mlx":
			return metadata.FormatMLX
		case "safetensors":
			return metadata.FormatSafetensors
		case "transformers":
			hasTransformers = true
		}
	}
	if hasTransformers {
		return metadata.FormatHuggingFace
	}
	return ""
}

// FormatSupported checks whether an app (given its declared formats from
// provider config) can handle the requiredFormat.
//
// Rules:
//   - requiredFormat == "": the model's format is unknown. The app is
//     compatible only if it declares at least one format (i.e. it is a local
//     inference app, not a cloud-only provider with no format declarations).
//   - requiredFormat != "": the app must explicitly list that format.
func FormatSupported(appFormats []string, requiredFormat string) bool {
	if requiredFormat == "" {
		// Format unknown — accept any app that declares formats
		// (apps with empty formats are cloud providers or unconfigured)
		return len(appFormats) > 0
	}
	for _, f := range appFormats {
		if strings.EqualFold(f, requiredFormat) {
			return true
		}
	}
	return false
}

// ProviderFormatEntry describes one installed provider and its declared formats,
// used to build diagnostic error messages.
type ProviderFormatEntry struct {
	Name    string
	Formats []string
}

// FormatIncompatibleError builds a diagnostic message for when no provider
// can serve a model. Both server-side and client-side error builders use this
// to avoid duplicating the formatting logic.
func FormatIncompatibleError(model, detectedFormat, repo string, providers []ProviderFormatEntry) string {
	formatDesc := detectedFormat
	if formatDesc == "" {
		formatDesc = "unknown"
	}

	// Deduplicate and sort for deterministic output.
	seen := make(map[string]bool)
	var parts []string
	for _, p := range providers {
		key := strings.ToLower(p.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(p.Formats) > 0 {
			parts = append(parts, fmt.Sprintf("%s (%s)", p.Name, strings.Join(p.Formats, ", ")))
		} else {
			parts = append(parts, p.Name)
		}
	}
	sort.Strings(parts)

	providersDesc := "none"
	if len(parts) > 0 {
		providersDesc = strings.Join(parts, ", ")
	}

	return fmt.Sprintf(
		"no compatible provider for %q (detected format: %s, repo: %s). "+
			"Installed providers: [%s]. "+
			"Install a provider that supports this format",
		model, formatDesc, repo, providersDesc)
}
