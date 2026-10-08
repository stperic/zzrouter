package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// StoreConfig is the interface the store needs from its config.
// Satisfied by config.PricingConfig.
type StoreConfig interface {
	IsEnabled() bool
	GetSource() string
	GetRefreshInterval() time.Duration
}

// Store is a thread-safe in-memory store for model pricing data.
// Construction is free — disk cache is lazy-loaded on the first read.
// Call Start(ctx) to run the background refresh goroutine.
type Store struct {
	config StoreConfig

	mu       sync.RWMutex
	models   map[string]ModelPricing
	metadata CacheMetadata

	cacheDir string
	loadOnce sync.Once // guards one-time disk-cache load

	stopMu     sync.Mutex
	cancelLoop context.CancelFunc // nil when not running
	done       chan struct{}      // closed when refresh loop exits

	missMu sync.Mutex
	misses map[string]int64 // "provider/model" → times seen un-priced

	// Operator overrides, guarded by mu alongside models so a lookup
	// sees one consistent view. See overrides.go.
	overrides     map[string]Override
	overridesPath string
}

// NewStore creates a new pricing store. No I/O happens until the first
// read (lazy disk-cache load) or until Start is called.
func NewStore(cfg StoreConfig, cacheDir string) *Store {
	return &Store{
		config:    cfg,
		models:    make(map[string]ModelPricing),
		cacheDir:  cacheDir,
		misses:    make(map[string]int64),
		overrides: make(map[string]Override),
	}
}

// maxTrackedMisses bounds the un-priced registry. Model names reaching
// it are attacker-influenced (any string a client puts in "model" that
// routes somewhere), so the map needs a ceiling; past it new keys are
// dropped and only the counters already being tracked keep moving.
// A few hundred distinct un-priced models is far past the point where
// the operator has a catalog problem worth acting on.
const maxTrackedMisses = 512

// RecordMiss notes that a request could not be priced for any of the
// given candidate models. The first sighting of a (provider, model)
// pair warns; repeats only bump the counter, because an un-priced model
// in steady traffic would otherwise flood the log. Read the tally back
// with Misses.
//
// A miss is not a benign zero: the request settles against the spend
// ledger at $0, so a virtual key with a budget cannot be stopped by it.
func (s *Store) RecordMiss(provider string, models ...string) {
	if s == nil {
		return
	}
	model := ""
	for _, m := range models {
		if m != "" {
			model = m
			break
		}
	}
	if model == "" {
		return
	}

	// Same canonical shape as Override.Key so an override can clear the
	// miss it answers; lookups are case-insensitive, so the lowercased
	// pair is the model's identity here.
	key := Override{Provider: provider, Model: model}.Key()

	s.missMu.Lock()
	if s.misses == nil {
		s.misses = make(map[string]int64)
	}
	_, seen := s.misses[key]
	if !seen && len(s.misses) >= maxTrackedMisses {
		s.missMu.Unlock()
		return
	}
	s.misses[key]++
	s.missMu.Unlock()

	if !seen {
		slog.Warn("No pricing data for model; request recorded at $0 and will not draw down any budget",
			"provider", provider, "model", model)
	}
}

// clearMiss drops a tallied miss, called when an override now answers it.
// The key shape matches Override.Key, which is the same "provider/model"
// shape RecordMiss builds.
func (s *Store) clearMiss(key string) {
	if s == nil {
		return
	}
	s.missMu.Lock()
	delete(s.misses, key)
	s.missMu.Unlock()
}

// Misses returns a copy of the un-priced tally, keyed "provider/model".
func (s *Store) Misses() map[string]int64 {
	if s == nil {
		return nil
	}
	s.missMu.Lock()
	defer s.missMu.Unlock()
	out := make(map[string]int64, len(s.misses))
	for k, v := range s.misses {
		out[k] = v
	}
	return out
}

// ensureLoaded triggers a one-time load from the disk cache. Safe to
// call from any goroutine — sync.Once serializes the first caller.
func (s *Store) ensureLoaded() {
	s.loadOnce.Do(func() {
		if err := s.loadFromDisk(); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("Failed to load pricing cache from disk", "error", err)
			}
		}
	})
}

// Start loads cached data from disk (if available), performs an initial
// network fetch, and runs the periodic refresh ticker in a background
// goroutine. Returns immediately; the goroutine exits when ctx is
// cancelled or Stop is called. Calling Start twice on a running store
// is a no-op.
func (s *Store) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.stopMu.Lock()
	if s.cancelLoop != nil {
		s.stopMu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.cancelLoop = cancel
	s.done = done
	s.stopMu.Unlock()

	s.ensureLoaded()
	go s.runRefreshLoop(loopCtx, done)
}

// Stop cancels the background refresh loop and waits for it to exit.
// Safe to call multiple times and safe to call when Start was never
// invoked.
func (s *Store) Stop() {
	if s == nil {
		return
	}
	s.stopMu.Lock()
	cancel := s.cancelLoop
	done := s.done
	s.cancelLoop = nil
	s.done = nil
	s.stopMu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (s *Store) runRefreshLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("PANIC recovered in pricing store", "panic", r, "stack", string(debug.Stack()))
		}
	}()

	s.refresh(ctx)

	ticker := time.NewTicker(s.config.GetRefreshInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.refresh(ctx)
		case <-ctx.Done():
			slog.Info("Stopping pricing data refresh (graceful shutdown)")
			return
		}
	}
}

// Lookup returns pricing for a model by its LiteLLM key (e.g. "gpt-4o").
// Case-insensitive — keys are lowercased at parse time.
func (s *Store) Lookup(model string) (ModelPricing, bool) {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if o, ok := s.lookupOverrideLocked("", model); ok {
		return o.Pricing(), true
	}
	p, ok := s.models[strings.ToLower(model)]
	return p, ok
}

// LookupByProvider returns pricing for a model scoped to a provider.
// Tries "provider/model" first, then common LiteLLM aliases (e.g. azure → azure_ai),
// then falls back to bare "model". All lookups are case-insensitive.
func (s *Store) LookupByProvider(provider, model string) (ModelPricing, bool) {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Operator intent outranks the upstream table everywhere, so the
	// number on /pricing/:model is the number that gets billed.
	if o, ok := s.lookupOverrideLocked(provider, model); ok {
		return o.Pricing(), true
	}

	prov := strings.ToLower(provider)
	mod := strings.ToLower(model)

	// Try provider-prefixed key first (e.g. "openai/gpt-4o")
	if p, ok := s.models[prov+"/"+mod]; ok {
		return p, true
	}
	// Try LiteLLM provider alias (e.g. azure → azure_ai)
	if alias, ok := litellmProviderAliases[prov]; ok {
		if p, ok := s.models[alias+"/"+mod]; ok {
			return p, true
		}
	}
	// Fall back to bare model name
	p, ok := s.models[mod]
	return p, ok
}

// litellmProviderAliases maps zzRouter provider names to their LiteLLM equivalents
// when the naming conventions differ.
var litellmProviderAliases = map[string]string{
	"azure": "azure_ai",
}

// Search returns all models whose key contains the given substring (case-insensitive).
// If provider is non-empty, results are filtered to that LiteLLM provider.
func (s *Store) Search(query string, provider string) map[string]ModelPricing {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := strings.ToLower(query)
	prov := strings.ToLower(provider)
	results := make(map[string]ModelPricing)
	for name, pricing := range s.models {
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			continue
		}
		if prov != "" && strings.ToLower(pricing.Provider) != prov {
			continue
		}
		results[name] = pricing
	}
	return results
}

// ModelCount returns the number of models in the store.
func (s *Store) ModelCount() int {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.models)
}

// Metadata returns the cache metadata (last fetch time, hash, etc).
func (s *Store) Metadata() CacheMetadata {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadata
}

// refresh fetches pricing data from the remote source and updates the store.
func (s *Store) refresh(ctx context.Context) {
	var prevMeta *CacheMetadata
	s.mu.RLock()
	if s.metadata.ContentHash != "" {
		m := s.metadata // value copy — prevMeta must not alias the live field
		prevMeta = &m
	}
	s.mu.RUnlock()

	result, err := fetch(ctx, s.config.GetSource(), prevMeta)
	if err != nil {
		// Cancellation during shutdown is expected — don't warn about it.
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		slog.Warn("Failed to refresh pricing data", "error", err)
		return
	}

	if !result.Changed {
		slog.Debug("Pricing data unchanged, skipping update")
		return
	}

	s.mu.Lock()
	s.models = result.Data
	s.metadata = result.Metadata
	s.mu.Unlock()

	// Persist to disk
	if err := s.saveToDisk(); err != nil {
		slog.Warn("Failed to persist pricing data to disk", "error", err)
	}
}

// diskCache is the on-disk format for persisted pricing data.
type diskCache struct {
	Metadata CacheMetadata           `json:"metadata"`
	Models   map[string]ModelPricing `json:"models"`
}

func (s *Store) cacheFilePath() string {
	return filepath.Join(s.cacheDir, "pricing-cache.json")
}

func (s *Store) saveToDisk() error {
	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	s.mu.RLock()
	cache := diskCache{
		Metadata: s.metadata,
		Models:   s.models,
	}
	s.mu.RUnlock()

	data, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}

	dest := s.cacheFilePath()
	if err := utils.AtomicWriteFile(dest, data, 0o644); err != nil {
		return fmt.Errorf("persist pricing cache: %w", err)
	}

	slog.Debug("Pricing data persisted to disk", "path", dest, "models", len(cache.Models))
	return nil
}

func (s *Store) loadFromDisk() error {
	path := s.cacheFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var cache diskCache
	if err := json.Unmarshal(data, &cache); err != nil {
		// Corrupt cache: rotate out of the way so the next refresh can
		// write a clean one, and start with an empty store.
		// On Windows os.Rename fails if target exists, so remove any
		// previous .corrupt file first.
		corrupt := path + ".corrupt"
		os.Remove(corrupt) // ignore error: may not exist
		if renameErr := os.Rename(path, corrupt); renameErr != nil {
			os.Remove(path) // last resort: can't rotate, just delete
		}
		slog.Warn("Corrupt pricing cache rotated, starting fresh", "error", err, "backup", corrupt)
		return nil
	}

	s.mu.Lock()
	s.models = cache.Models
	s.metadata = cache.Metadata
	s.mu.Unlock()

	slog.Info("Loaded pricing data from disk cache", "models", len(cache.Models), "fetched_at", cache.Metadata.FetchedAt)
	return nil
}
