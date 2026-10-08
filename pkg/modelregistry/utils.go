package modelregistry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/stperic/zzrouter/pkg/config"
)

// Thread-safe overrides for testing and config
var (
	modelsRootDirOverride   string
	modelsRootDirOverrideMu sync.RWMutex

	// modelsConfigCache is the local cache directory from node.yaml models.cache
	modelsConfigCache   string
	modelsConfigCacheMu sync.RWMutex

	// modelsConfigShared is the shared storage path from node.yaml models.shared
	modelsConfigShared   string
	modelsConfigSharedMu sync.RWMutex
)

func getModelsRootDirOverride() string {
	modelsRootDirOverrideMu.RLock()
	defer modelsRootDirOverrideMu.RUnlock()
	return modelsRootDirOverride
}

// SetModelsRootDirOverride overrides the models root directory (for testing).
func SetModelsRootDirOverride(dir string) {
	modelsRootDirOverrideMu.Lock()
	defer modelsRootDirOverrideMu.Unlock()
	modelsRootDirOverride = dir
}

// ClearModelsRootDirOverride removes the models root directory override.
func ClearModelsRootDirOverride() {
	modelsRootDirOverrideMu.Lock()
	defer modelsRootDirOverrideMu.Unlock()
	modelsRootDirOverride = ""
}

// SetModelsConfig sets the models configuration from node.yaml. The
// `shared` value is consumed by GetModelsRootDir as a discovery-root
// hint (priority 2) — it does NOT change cluster fan-out behavior.
// The "skip worker fan-out / collapse to coord" optimizations that
// once keyed off this flag were retired because they were unsound for
// daemon-mediated providers (Ollama keeps its blob store under each
// host's `~/.ollama/`, never on the shared mount).
func SetModelsConfig(cache, shared string) {
	modelsConfigCacheMu.Lock()
	modelsConfigCache = cache
	modelsConfigCacheMu.Unlock()

	modelsConfigSharedMu.Lock()
	modelsConfigShared = shared
	modelsConfigSharedMu.Unlock()
}

func getModelsConfigCache() string {
	modelsConfigCacheMu.RLock()
	defer modelsConfigCacheMu.RUnlock()
	return modelsConfigCache
}

func getSharedDir() string {
	modelsConfigSharedMu.RLock()
	defer modelsConfigSharedMu.RUnlock()
	return modelsConfigShared
}

// validateModelDir checks that a model directory path is safe to use.
// It must be an absolute path and must not contain ".." components.
func validateModelDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("ZZROUTER_MODEL_DIR must be an absolute path, got: %s", dir)
	}
	if strings.Contains(dir, "..") {
		return fmt.Errorf("ZZROUTER_MODEL_DIR must not contain '..', got: %s", dir)
	}
	return nil
}

// GetModelsRootDir returns the root directory for model discovery/scanning.
// Priority: override > env > shared > cache > PathResolver default
func GetModelsRootDir() (string, error) {
	// Priority 0: Thread-safe override (for testing)
	if override := getModelsRootDirOverride(); override != "" {
		return override, nil
	}

	// Priority 1: Explicit environment variable override
	if envDir := os.Getenv("ZZROUTER_MODEL_DIR"); envDir != "" {
		if err := validateModelDir(envDir); err != nil {
			return "", err
		}
		return envDir, nil
	}

	// Priority 2: Shared storage (for model discovery on workers)
	if shared := getSharedDir(); shared != "" {
		return shared, nil
	}

	// Priority 3: Config file models.cache
	if configCache := getModelsConfigCache(); configCache != "" {
		return configCache, nil
	}

	// Priority 4: Use PathResolver (handles platform/context detection)
	return config.Paths().GetModelsDir(), nil
}

// GetModelsCacheDir returns the local cache directory for models.
// Unlike GetModelsRootDir(), this never returns shared storage.
func GetModelsCacheDir() (string, error) {
	// Priority 0: Thread-safe override (for testing)
	if override := getModelsRootDirOverride(); override != "" {
		return override, nil
	}

	// Priority 1: Explicit environment variable override
	if envDir := os.Getenv("ZZROUTER_MODEL_DIR"); envDir != "" {
		if err := validateModelDir(envDir); err != nil {
			return "", err
		}
		return envDir, nil
	}

	// Priority 2: Config file models.cache (local cache, NOT shared)
	if configCache := getModelsConfigCache(); configCache != "" {
		return configCache, nil
	}

	// Priority 3: Use PathResolver (handles platform/context detection)
	return config.Paths().GetModelsDir(), nil
}

// --- Model Path Types ---

// ModelPath represents a complete path to a model
type ModelPath struct {
	Organization string
	ModelName    string
	Format       string
	FullPath     string
	FileName     string // For file-based formats (GGUF)
}

// MatchesModelPattern checks if a model name matches a pattern (supports wildcards)
func MatchesModelPattern(modelName, pattern string) bool {
	if pattern == "" || modelName == "" {
		return false
	}

	if strings.EqualFold(pattern, modelName) {
		return true
	}

	if pattern == "*" {
		return true
	}

	modelLower := strings.ToLower(modelName)
	patternLower := strings.ToLower(pattern)

	if strings.Contains(patternLower, "*") {
		parts := strings.Split(patternLower, "*")

		if len(parts) == 2 {
			prefix := parts[0]
			suffix := parts[1]

			if prefix != "" && suffix == "" {
				return strings.HasPrefix(modelLower, prefix)
			}
			if prefix == "" && suffix != "" {
				return strings.HasSuffix(modelLower, suffix)
			}
			if prefix != "" && suffix != "" {
				return strings.HasPrefix(modelLower, prefix) && strings.HasSuffix(modelLower, suffix)
			}
		}
	}

	return false
}
