package modelregistry

import (
	"context"
	"fmt"
	"maps"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/filesystem"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/ollama"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ListAllModels returns models from ALL sources (Ollama, HuggingFace, standalone files).
//
// Backed by an EventCache: the underlying scan (Ollama API + filesystem)
// runs once and its result is reused until InvalidateScanCache is called.
// Concurrent callers coalesce onto a single in-flight scan via the write
// lock, so a burst of requests costs one scan, not N.
//
// The returned slice is shared with the cache — callers that need to
// sort or mutate the result must copy first (see ListAllModelsCopy).
func (r *Registry) ListAllModels() ([]*metadata.ModelMetadata, error) {
	models, _, err := r.ModelSnapshot(context.Background())
	return models, err
}

// ModelSnapshot captures models and failed provider inventories from the same scan.
func (r *Registry) ModelSnapshot(ctx context.Context) ([]*metadata.ModelMetadata, map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	r.scanMu.RLock()
	if r.scanValid {
		models := r.scanModels
		failures := maps.Clone(r.scanFailures)
		r.scanMu.RUnlock()
		return models, failures, nil
	}
	r.scanMu.RUnlock()

	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// Double-check after upgrading: another goroutine may have scanned
	// between our RUnlock and Lock.
	if r.scanValid {
		return r.scanModels, maps.Clone(r.scanFailures), nil
	}

	models, err := r.buildModelList(ctx)
	if err != nil {
		// Leave the cache invalid so the next call retries. Callers
		// that get the error should surface it; future calls won't be
		// poisoned with a partial result.
		return nil, nil, err
	}
	r.scanModels = models
	r.scanValid = true
	return models, maps.Clone(r.scanFailures), nil
}

// InvalidateScanCache marks the scan cache stale. The next ListAllModels
// call will re-scan all sources. Cheap (takes the scan write lock only
// long enough to flip a flag) and safe to call from any goroutine.
//
// Call this after any mutation that changes the model catalog:
// pull completion, model delete, apps-config reload. The server layer
// cascades this from its ModelCache invalidation path — external callers
// outside the server generally don't need to call it directly.
func (r *Registry) InvalidateScanCache() {
	r.scanMu.Lock()
	r.scanModels = nil
	r.scanFailures = nil
	r.scanValid = false
	r.scanMu.Unlock()
}

// buildModelList builds the model list from all sources.
// Called under r.scanMu held for writing — not safe to call from elsewhere.
func (r *Registry) buildModelList(ctx context.Context) ([]*metadata.ModelMetadata, error) {
	var allModels []*metadata.ModelMetadata
	r.scanFailures = make(map[string]string)

	// 1. Get models from every enabled external provider that speaks the
	// Ollama protocol. This covers both the managed `ollama` install and
	// any runtime-added connect instances (`ollama-nas`, `ollama-lab`, …).
	// Each backend's models are tagged with that provider's key as
	// AssignedApp, so the routing pipeline can dispatch per-backend without
	// hardcoded name lookups.
	//
	// Ollama models are managed entirely by each daemon — zzRouter stores
	// nothing locally for them. All state lives behind the /api/tags call.
	//
	ollamaCtx, cancel := context.WithTimeout(ctx, ollama.APITimeout)
	defer cancel()
	if cfg := r.appsConfigFn(); cfg != nil {
		resolver := backend.NewResolver(r.appsConfigFn)
		for name, p := range cfg.ExternalProviders() {
			if p.Protocol != config.ProtocolOllama {
				continue
			}
			daemon, ok := resolver.Resolve(name)
			if !ok {
				continue
			}
			models, err := r.ollamaConnector.ListTags(ollamaCtx, daemon, name)
			if err != nil {
				utils.LogWarnf("Failed to list Ollama models from %s (%s): %v", name, daemon.Endpoint, err)
				reason := utils.SanitizeErrorMessage(security.RedactSensitive(err.Error()))
				if len(reason) > 1024 {
					reason = reason[:1024]
				}
				r.scanFailures[name] = reason
				continue
			}
			allModels = append(allModels, models...)
		}
	}

	// 2. Get HuggingFace models from filesystem scanner
	// These are scanned dynamically
	hfModels, err := r.huggingfaceConnector.ScanModels()
	if err == nil {
		allModels = append(allModels, hfModels...)
	} else {
		utils.LogWarnf("Failed to scan HuggingFace models: %v", err)
	}

	// 3. Get GGUF and other standalone models from filesystem scanner
	// These are scanned dynamically (replaces index-based approach)
	standaloneModels, err := filesystem.ScanModels(r.modelsRoot)
	if err == nil {
		allModels = append(allModels, standaloneModels...)
	} else {
		utils.LogWarnf("Failed to scan standalone models: %v", err)
	}

	// 4. Final deduplication pass: Remove any duplicates within allModels by FullPath
	// This handles cases where the same model might be added multiple times
	allModels = deduplicateByFullPath(allModels)

	// 5. Auto-assign apps for models without assignments
	r.autoAssignApps(allModels)

	// Sort by name for consistent output
	sortModels(allModels, SortByName, true)

	return allModels, nil
}

// deduplicateByFullPath removes duplicate models from a list based on FullPath.
// Also matches by Name+SourceID for HuggingFace models to catch cases where
// the same model has different FullPath values (e.g., directory vs file path).
func deduplicateByFullPath(models []*metadata.ModelMetadata) []*metadata.ModelMetadata {
	if len(models) == 0 {
		return models
	}

	seenByPath := make(map[string]bool, len(models))
	seenByNameAndSource := make(map[string]bool) // Key: "Name|SourceID|SourceRepo"
	result := make([]*metadata.ModelMetadata, 0, len(models))

	for _, m := range models {
		if m == nil {
			continue
		}

		// Primary deduplication: Use FullPath as the unique key
		pathKey := m.FullPath
		if pathKey == "" {
			pathKey = m.Name
		}

		// Secondary deduplication: For HuggingFace models, also check Name+SourceID
		// This catches cases where the same model has different FullPath values
		// (e.g., directory path from scanner vs file path from index)
		nameSourceKey := ""
		if m.SourceRepo == metadata.SourceHuggingFace && m.Name != "" && m.SourceID != "" {
			nameSourceKey = fmt.Sprintf("%s|%s|%s", m.Name, m.SourceID, m.SourceRepo)
		}

		seen := seenByPath[pathKey]
		if !seen && nameSourceKey != "" {
			seen = seenByNameAndSource[nameSourceKey]
		}

		if !seen {
			seenByPath[pathKey] = true
			if nameSourceKey != "" {
				seenByNameAndSource[nameSourceKey] = true
			}
			result = append(result, m)
		}
		// Skip duplicates (keep first occurrence)
	}

	return result
}

// ListAllModelsCopy returns a defensive copy of all models.
// Use this if you need to modify the returned slice.
// Note: This is slower than ListAllModels() due to the copy operation.
func (r *Registry) ListAllModelsCopy() ([]*metadata.ModelMetadata, error) {
	models, err := r.ListAllModels()
	if err != nil {
		return nil, err
	}

	result := make([]*metadata.ModelMetadata, len(models))
	copy(result, models)
	return result, nil
}

// withModels gets models and applies a function.
// DRY helper that avoids repeating the ListAllModels() call and error pattern.
func (r *Registry) withModels(fn func([]*metadata.ModelMetadata) error) error {
	models, err := r.ListAllModels()
	if err != nil {
		return err
	}
	if models == nil {
		models = []*metadata.ModelMetadata{} // Ensure non-nil slice
	}
	return fn(models)
}

// filterModels gets models and filters them, returning the filtered results.
// DRY helper paired with withModels.
func (r *Registry) filterModels(filter func(*metadata.ModelMetadata) bool) ([]*metadata.ModelMetadata, error) {
	models, err := r.ListAllModels()
	if err != nil {
		return nil, err
	}

	if len(models) == 0 {
		return []*metadata.ModelMetadata{}, nil
	}

	// Pre-allocate with estimated capacity (assume ~50% match rate).
	// Use at least 4 to avoid tiny allocations.
	capacity := max(len(models)/2, 4)
	result := make([]*metadata.ModelMetadata, 0, capacity)
	for _, m := range models {
		if m != nil && filter(m) {
			result = append(result, m)
		}
	}
	return result, nil
}

// ListModels returns all models in the registry (alias for ListAllModels).
func (r *Registry) ListModels() ([]*metadata.ModelMetadata, error) {
	return r.ListAllModels()
}

// Rescan performs a full filesystem rescan (invalidates the scan cache;
// the next ListAllModels will re-scan Ollama + HF + standalone). Server
// callers (e.g., RescanLocal) should additionally invalidate the
// cluster-join ModelCache so the new scan result propagates to readers.
func (r *Registry) Rescan() error {
	r.InvalidateScanCache()
	return nil
}

// autoAssignApps automatically assigns apps to models based on format.
// AutoAssignProvider handles all validation and logging.
func (r *Registry) autoAssignApps(models []*metadata.ModelMetadata) {
	if r.appsConfigFn == nil || r.appsConfigFn() == nil {
		return
	}

	for _, model := range models {
		// Skip if already assigned (e.g., Ollama models are pre-assigned).
		if model.AssignedApp != "" {
			continue
		}

		// Skip if no format detected.
		if model.Format == "" {
			continue
		}

		provider, host := AutoAssignProvider(model.Name, model.Format, r.appsConfigFn())
		if provider != "" {
			model.AssignedApp = provider
			model.AssignedNode = host
			model.AssignmentSource = "auto"
			model.AssignedAt = utils.Now()
		}
	}
}
