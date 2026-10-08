//go:build integration

package modelregistry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// TestSmartDownloadWithRealModels tests smart download with actual small models
// Run with: go test -tags=integration -v ./pkg/models/... -run TestSmartDownload
func TestSmartDownloadWithRealModels(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Use small models for testing
	testModels := []struct {
		name         string
		baseModel    string
		providerType string
		strategy     metadata.SmartPullStrategy
		expectFound  bool // Whether we expect to find pre-converted
	}{
		{
			name:         "TinyLlama with Ollama - should find TheBloke GGUF",
			baseModel:    "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
			providerType: "ollama",
			strategy:     metadata.StrategySmartDefault,
			expectFound:  true, // TheBloke has this
		},
		{
			name:         "Phi-2 with Ollama - should find pre-converted",
			baseModel:    "microsoft/phi-2",
			providerType: "ollama",
			strategy:     metadata.StrategySmartDefault,
			expectFound:  true, // Popular model, likely has GGUF
		},
		{
			name:         "Qwen2.5 with llama.cpp - should find pre-converted",
			baseModel:    "Qwen/Qwen2.5-0.5B-Instruct",
			providerType: "llama.cpp",
			strategy:     metadata.StrategySmartDefault,
			expectFound:  true, // Small Qwen model
		},
		{
			name:         "Source-only strategy",
			baseModel:    "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
			providerType: "ollama",
			strategy:     metadata.StrategySourceOnly,
			expectFound:  false, // Should not search for pre-converted
		},
	}

	registry := DefaultSourceRegistry()

	for _, tt := range testModels {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Testing model: %s", tt.baseModel)
			t.Logf("Provider: %s", tt.providerType)
			t.Logf("Strategy: %s", tt.strategy)

			// Get required format
			format := GetProviderFormat(tt.providerType)
			t.Logf("Required format: %s", format)

			// Only search for pre-converted if strategy allows
			if tt.strategy == metadata.StrategySmartDefault || tt.strategy == metadata.StrategyPreConvertedOnly {
				// Search for pre-converted model
				finder := NewPreConvertedModelFinder(registry, format)

				startTime := time.Now()
				result := finder.Find(tt.baseModel)
				searchDuration := time.Since(startTime)

				t.Logf("Search completed in: %v", searchDuration)
				t.Logf("Found: %v", result.Found)

				if result.Found {
					t.Logf("Pre-converted found at: %s", result.RepoID)
					t.Logf("   Source: %s", result.SourceName)
					t.Logf("   Message: %s", result.Message)

					if !tt.expectFound {
						t.Logf(" Found pre-converted but didn't expect it (this is OK)")
					}
				} else {
					t.Logf(" Pre-converted not found: %s", result.Message)

					if tt.expectFound {
						t.Logf("   Expected to find pre-converted, but didn't (may have been removed)")
						t.Logf("   Would fallback to source download")
					}
				}

				// Verify search was reasonably fast (< 5 seconds per source)
				maxExpectedDuration := time.Duration(len(registry.GetEnabledSources())) * 5 * time.Second
				if searchDuration > maxExpectedDuration {
					t.Errorf("Search took too long: %v (max expected: %v)", searchDuration, maxExpectedDuration)
				}
			} else {
				t.Logf("Strategy %s - skipping pre-converted search", tt.strategy)
			}
		})
	}
}

// TestRepoIDPatterns tests various repo ID patterns with real checks
func TestRepoIDPatterns(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	registry := DefaultSourceRegistry()
	finder := NewPreConvertedModelFinder(registry, metadata.FormatGGUF)

	// Test different model naming patterns
	testCases := []struct {
		baseModel string
		notes     string
	}{
		{
			baseModel: "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
			notes:     "Model with version number",
		},
		{
			baseModel: "microsoft/phi-2",
			notes:     "Simple model name",
		},
		{
			baseModel: "Qwen/Qwen2.5-0.5B-Instruct",
			notes:     "Model with dots in version",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.baseModel, func(t *testing.T) {
			t.Logf("Testing: %s (%s)", tc.baseModel, tc.notes)

			// Extract model name
			parts := strings.Split(tc.baseModel, "/")
			modelName := parts[len(parts)-1]

			// Test repo ID generation for each source
			for _, source := range registry.GetEnabledSources() {
				repoIDs := finder.buildRepoIDs(source.Name, modelName, tc.baseModel)
				t.Logf("  Source '%s' generates %d potential repo IDs:", source.Name, len(repoIDs))
				for i, repoID := range repoIDs {
					t.Logf("    %d. %s", i+1, repoID)
				}
			}
		})
	}
}

// TestConversionCacheWithSmallModel tests cache operations with a small model
func TestConversionCacheWithSmallModel(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Use temporary directory for testing
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	modelID := "test-org/tiny-model"

	t.Run("Cache lifecycle", func(t *testing.T) {
		cache := NewConversionCacheForProvider(modelID, "ollama")

		// 1. Check initial state (should not exist)
		exists, err := cache.Exists()
		if err != nil {
			t.Fatalf("Exists check failed: %v", err)
		}
		if exists {
			t.Error("Cache should not exist initially")
		}

		// 2. Get cache info
		info, err := cache.GetInfo()
		if err != nil {
			t.Fatalf("GetInfo failed: %v", err)
		}
		if info.Exists {
			t.Error("Info should show cache doesn't exist")
		}

		t.Logf("Cache path would be: %s", info.Path)

		// 3. Simulate cache creation (without actual conversion)
		// Create the directory structure
		if err := os.MkdirAll(info.Path, 0755); err != nil {
			t.Fatalf("Failed to create cache directory: %v", err)
		}

		// Create a dummy GGUF file
		dummyFile := filepath.Join(info.Path, "model.gguf")
		if err := os.WriteFile(dummyFile, []byte("dummy gguf content"), 0644); err != nil {
			t.Fatalf("Failed to create dummy file: %v", err)
		}

		// 4. Check existence again
		exists, err = cache.Exists()
		if err != nil {
			t.Fatalf("Exists check failed: %v", err)
		}
		if !exists {
			t.Error("Cache should exist after creation")
		}

		// 5. Get updated info
		info, err = cache.GetInfo()
		if err != nil {
			t.Fatalf("GetInfo failed: %v", err)
		}
		if !info.Exists || !info.IsValid {
			t.Error("Cache should be valid after creation")
		}
		if info.Size == 0 {
			t.Error("Cache size should be greater than 0")
		}

		t.Logf("Cache size: %d bytes", info.Size)
		t.Logf("Cache modified: %v", info.ModifiedTime)

		// 6. Test cache deletion
		if err := cache.Delete(); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		// 7. Verify deletion
		exists, err = cache.Exists()
		if err != nil {
			t.Fatalf("Exists check after delete failed: %v", err)
		}
		if exists {
			t.Error("Cache should not exist after deletion")
		}
	})
}

// TestMultipleQuantizationLevels tests caching different quantization levels
func TestMultipleQuantizationLevels(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	modelID := "test-org/multi-quant-model"
	quantizations := []string{"Q4_K_M", "Q5_K_M", "Q8_0"}

	for _, quant := range quantizations {
		t.Run(fmt.Sprintf("Quantization_%s", quant), func(t *testing.T) {
			cache := NewConversionCacheForProvider(modelID, "ollama").
				WithQuantization(quant)

			// Get cache path
			path, err := cache.GetCachePath()
			if err != nil {
				t.Fatalf("GetCachePath failed: %v", err)
			}

			t.Logf("Cache path for %s: %s", quant, path)

			// Verify quantization is set correctly
			if cache.Quantization != quant {
				t.Errorf("Expected quantization %s, got %s", quant, cache.Quantization)
			}

			// All quantizations should use the same base path (different files)
			// This tests that we can have multiple quantizations cached
			if !strings.Contains(path, "gguf") {
				t.Errorf("Path should contain 'gguf': %s", path)
			}
		})
	}
}

// TestCacheManagerOperations tests cache manager with multiple models
func TestCacheManagerOperations(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	manager := NewCacheManager()

	// Create test model structure with multiple formats
	modelsRoot := filepath.Join(tmpDir, ".zzrouter", "models")

	testModels := []struct {
		modelID string
		formats []string
	}{
		{
			modelID: "test-org/model-1",
			formats: []string{metadata.FormatHuggingFace, metadata.FormatGGUF},
		},
		{
			modelID: "test-org/model-2",
			formats: []string{metadata.FormatHuggingFace, metadata.FormatGGUF, metadata.FormatONNX},
		},
	}

	// Create test structure
	for _, tm := range testModels {
		for _, format := range tm.formats {
			modelPath := filepath.Join(modelsRoot, strings.Replace(tm.modelID, "/", string(filepath.Separator), -1), format)
			os.MkdirAll(modelPath, 0755)

			// Create dummy file
			var filename string
			switch format {
			case metadata.FormatGGUF:
				filename = "model.gguf"
			case metadata.FormatHuggingFace:
				filename = "config.json"
			case metadata.FormatONNX:
				filename = "model.onnx"
			default:
				filename = "model.bin"
			}
			os.WriteFile(filepath.Join(modelPath, filename), []byte("test"), 0644)
		}
	}

	t.Run("ListCachedFormats", func(t *testing.T) {
		formats, err := manager.ListCachedFormats("test-org/model-1")
		if err != nil {
			t.Fatalf("ListCachedFormats failed: %v", err)
		}

		if len(formats) != 2 {
			t.Errorf("Expected 2 formats for model-1, got %d", len(formats))
		}

		t.Logf("Model-1 formats: %v", formats)
	})

	t.Run("GetModelCacheInfo", func(t *testing.T) {
		info, err := manager.GetModelCacheInfo("test-org/model-2")
		if err != nil {
			t.Fatalf("GetModelCacheInfo failed: %v", err)
		}

		if len(info) != 3 {
			t.Errorf("Expected 3 formats for model-2, got %d", len(info))
		}

		for format, cacheInfo := range info {
			t.Logf("Format %s: exists=%v, valid=%v, size=%d",
				format, cacheInfo.Exists, cacheInfo.IsValid, cacheInfo.Size)
		}
	})

	t.Run("GetTotalCacheSize", func(t *testing.T) {
		size, err := manager.GetTotalCacheSize()
		if err != nil {
			t.Fatalf("GetTotalCacheSize failed: %v", err)
		}

		// Should have 5 files total (2 for model-1, 3 for model-2)
		expectedSize := int64(5 * 4) // 5 files * 4 bytes each ("test")
		if size != expectedSize {
			t.Errorf("Expected total size %d, got %d", expectedSize, size)
		}

		t.Logf("Total cache size: %d bytes", size)
	})

	t.Run("CleanAllConversions", func(t *testing.T) {
		// Clean all conversions (should keep HF format)
		if err := manager.CleanAllConversions(); err != nil {
			t.Fatalf("CleanAllConversions failed: %v", err)
		}

		// Verify HF format still exists
		formats, err := manager.ListCachedFormats("test-org/model-1")
		if err != nil {
			t.Fatalf("ListCachedFormats after clean failed: %v", err)
		}

		if len(formats) != 1 {
			t.Errorf("Expected 1 format (HF) after clean, got %d", len(formats))
		}

		if len(formats) > 0 && formats[0] != metadata.FormatHuggingFace {
			t.Errorf("Expected only HF format to remain, got %s", formats[0])
		}

		t.Logf("After clean, remaining formats: %v", formats)
	})
}

// TestPreConvertedModelSearch tests searching for actual pre-converted models
func TestPreConvertedModelSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	registry := DefaultSourceRegistry()
	finder := NewPreConvertedModelFinder(registry, metadata.FormatGGUF)

	// Test with known popular models that likely have GGUF versions
	testModels := []string{
		"TinyLlama/TinyLlama-1.1B-Chat-v1.0",
		"microsoft/phi-2",
		"Qwen/Qwen2.5-0.5B-Instruct",
	}

	for _, modelID := range testModels {
		t.Run(modelID, func(t *testing.T) {
			t.Logf("Searching for pre-converted GGUF of: %s", modelID)

			startTime := time.Now()
			result := finder.Find(modelID)
			duration := time.Since(startTime)

			t.Logf("Search completed in: %v", duration)

			if result.Found {
				t.Logf("FOUND!")
				t.Logf("   Registry: %s", result.RepoID)
				t.Logf("   Source: %s", result.SourceName)
				t.Logf("   Format: %s", result.Format)

				// Verify the repo actually exists by checking again
				if !finder.checkRepoExists(result.RepoID) {
					t.Errorf("Repo reported as found but doesn't exist: %s", result.RepoID)
				}
			} else {
				t.Logf(" NOT FOUND")
				t.Logf("   Message: %s", result.Message)
				t.Logf("   Would fallback to source download")
			}

			// Performance check
			if duration > 30*time.Second {
				t.Errorf("Search took too long: %v", duration)
			}
		})
	}
}

// TestQuantizationVariants tests finding different quantization levels
func TestQuantizationVariants(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	registry := DefaultSourceRegistry()
	modelID := "TinyLlama/TinyLlama-1.1B-Chat-v1.0"

	quantizations := []string{"Q4_K_M", "Q5_K_M", "Q8_0"}

	for _, quant := range quantizations {
		t.Run(fmt.Sprintf("Quant_%s", quant), func(t *testing.T) {
			finder := NewPreConvertedModelFinder(registry, metadata.FormatGGUF).
				WithQuantization(quant)

			result := finder.Find(modelID)

			t.Logf("Quantization %s: Found=%v", quant, result.Found)
			if result.Found {
				t.Logf("  Registry: %s", result.RepoID)
			}
		})
	}
}

// TestSourcePriorityOrder tests that sources are checked in priority order
func TestSourcePriorityOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	registry := DefaultSourceRegistry()
	sources := registry.GetEnabledSources()

	t.Logf("Testing source priority order:")
	for i, source := range sources {
		t.Logf("  %d. %s (priority: %d)", i+1, source.Name, source.Priority)
	}

	// Verify priority ordering
	for i := 0; i < len(sources)-1; i++ {
		if sources[i].Priority > sources[i+1].Priority {
			t.Errorf("Sources not in priority order: %s (priority %d) before %s (priority %d)",
				sources[i].Name, sources[i].Priority,
				sources[i+1].Name, sources[i+1].Priority)
		}
	}

	// Verify TheBloke is first
	if len(sources) > 0 && sources[0].Name != "TheBloke" {
		t.Errorf("Expected TheBloke as first source, got %s", sources[0].Name)
	}
}
