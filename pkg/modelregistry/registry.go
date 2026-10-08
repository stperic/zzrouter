package modelregistry

import (
	"errors"
	"fmt"
	"sync"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/ollama"
)

// Registry manages the catalog of downloaded models and queries apps.
//
// Registry holds per-source connectors (Ollama, HuggingFace, filesystem)
// and forwards calls to them. Source-specific config knowledge lives
// inside each connector — Registry stays provider-agnostic.
//
// ListAllModels is backed by an EventCache: results are cached until
// InvalidateScanCache is called. There is no TTL — mutations that can
// change the model catalog (pull completion, model delete, apps-config
// reload) must invalidate. The server layer (internal/server) cascades
// invalidation from its ModelCache so registry callers outside the
// server layer still get a fresh scan after any cluster-observable
// mutation.
//
// File layout (under pkg/modelregistry/):
//   - registry.go            — this file: struct + lifecycle (Stop, Reload).
//   - registry_scan.go       — ListAllModels, buildModelList, scan cache.
//   - registry_pull.go       — PullOllamaModelAsync, DownloadHuggingFaceModelAsync.
//   - registry_delete.go     — GetModel, DeleteModelByNameAndSource, validators.
//   - operations.go          — DeleteModel, VerifyModel, GetTotalDiskUsage, GetLargestModels.
//   - filter.go              — ModelFilter + GetStats + RegistryStats.
//   - pattern.go             — MatchPattern wildcard implementation.
type Registry struct {
	modelsRoot           string
	ollamaConnector      *ollama.Connector
	huggingfaceConnector *huggingface.Connector
	appsConfigFn         func() *config.AppsConfig // Always returns current config

	// Scan cache (EventCache — event-invalidated, no TTL).
	// scanMu guards scanModels and scanValid; a full scan runs under
	// the write lock so concurrent callers coalesce onto one scan.
	scanMu       sync.RWMutex
	scanModels   []*metadata.ModelMetadata
	scanValid    bool
	scanFailures map[string]string

	// Async-download lifecycle. stopMu serializes the entry check +
	// wg.Add window in the *Async methods against Stop's wg.Wait. Once
	// stopped is true, no new goroutine is spawned and pending wg.Wait
	// is not racing wg.Add (which would panic with "WaitGroup is reused
	// before previous Wait has returned").
	stopMu  sync.Mutex
	stopped bool
	wg      sync.WaitGroup

	// reloadMu guards lastReloadFP. ReloadConfig compares a new
	// AppsConfig fingerprint against the last-applied one and
	// short-circuits with DispositionIgnored on no-op reloads.
	reloadMu     sync.Mutex
	lastReloadFP [32]byte
}

// NewRegistry creates a registry and seeds it from the current
// AppsConfig exposed by appsConfigFn. The constructor reuses the
// runtime hot-reload path (ReloadConfig) for the initial seed so
// startup and post-install reloads share a single code path — there is
// no "first time is special" branch anywhere. Pass a non-nil
// appsConfigFn: an unconfigured accessor yields an inactive Ollama
// source, which is safe but surprising.
func NewRegistry(appsConfigFn func() *config.AppsConfig) (*Registry, error) {
	modelsRoot, err := GetModelsRootDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get models root: %w", err)
	}

	r := &Registry{
		modelsRoot:           modelsRoot,
		ollamaConnector:      ollama.NewConnector(),
		huggingfaceConnector: huggingface.NewConnector(modelsRoot),
		appsConfigFn:         appsConfigFn,
	}
	if appsConfigFn != nil {
		r.ReloadConfig(appsConfigFn())
	}
	return r, nil
}

// ReloadConfig fans the new AppsConfig out to every model-source
// connector that derives state from it. Each connector owns its own
// config-reading logic; Registry just dispatches. Adding a new
// reloader-aware connector is a one-line change here — there are no
// provider-specific branches.
//
// This signature matches ProviderAppManager.ReloadConfig so the server's
// config listener can drive both subsystems through a single interface.
func (r *Registry) ReloadConfig(cfg *config.AppsConfig) config.ReloadDisposition {
	fp := cfg.Fingerprint()
	r.reloadMu.Lock()
	unchanged := r.lastReloadFP == fp
	r.lastReloadFP = fp
	r.reloadMu.Unlock()
	if unchanged {
		return config.DispositionIgnored
	}
	r.ollamaConnector.ReloadConfig(cfg)
	// AssignedApp / AssignedNode on cached entries are derived from
	// appsConfig; a config change can shift those without any
	// filesystem change. Invalidate so the next scan re-runs
	// autoAssignApps against the new config.
	r.InvalidateScanCache()
	return config.DispositionApplied
}

// OllamaConnector returns the Ollama source connector. Callers that
// need the endpoint, status, or any other Ollama-specific detail go
// through the returned type — Registry itself exposes no ollama-named
// methods so its public API stays provider-agnostic.
func (r *Registry) OllamaConnector() *ollama.Connector {
	return r.ollamaConnector
}

// GetHuggingFaceConnector returns the HuggingFace connector for direct
// access.
func (r *Registry) GetHuggingFaceConnector() *huggingface.Connector {
	return r.huggingfaceConnector
}

// ErrRegistryStopped is the statusUpdater payload when an *Async
// download is refused because Stop has run. errors.Is friendly.
var ErrRegistryStopped = errors.New("modelregistry: registry stopped")

// beginAsyncDownload gates and reserves a wg slot under stopMu — the
// check + wg.Add MUST be atomic with Stop's set-flag, otherwise wg.Add
// can race wg.Wait at counter==0 ("WaitGroup is reused before previous
// Wait has returned"). Returns true → caller spawns and defers
// r.wg.Done(). Returns false → caller refuses via statusUpdater.
//
// Only the two *Async methods in registry_pull.go (PullOllamaModelAsync,
// DownloadHuggingFaceModelAsync) are expected to call this. Any future
// async path must gate on beginAsyncDownload + defer r.wg.Done() in the
// spawned goroutine — otherwise Stop's wg.Wait will deadlock or the
// goroutine will outlive Stop.
func (r *Registry) beginAsyncDownload() bool {
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	if r.stopped {
		return false
	}
	r.wg.Add(1)
	return true
}

// Stop refuses new async-download spawns and waits for in-flight ones
// to exit. Callers SHOULD cancel the per-download ctxs first — Stop
// owns the lifecycle flag and wait, not the download ctx. Idempotent.
func (r *Registry) Stop() {
	r.stopMu.Lock()
	r.stopped = true
	r.stopMu.Unlock()
	r.wg.Wait()
}
