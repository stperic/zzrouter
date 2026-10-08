package protocol

import (
	"fmt"
	"maps"
	"net/http"
	"sync"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// Registry loads provider implementations from provider config.
// Single source of truth — no hardcoded providers.
// Thread-safe for concurrent access.
type Registry struct {
	providers  map[string]FullProvider
	httpClient *http.Client
	mu         sync.RWMutex
}

// NewRegistry creates a registry from apps config.
// httpClient is the shared pooled client for all provider HTTP calls.
// If nil, providers create their own default clients (for tests).
func NewRegistry(cfg *config.AppsConfig, httpClient *http.Client) *Registry {
	reg := &Registry{
		providers:  make(map[string]FullProvider),
		httpClient: httpClient,
	}
	if cfg == nil {
		return reg
	}
	for key, p := range cfg.AllProviders() {
		if p.IsExplicitlyDisabled() || p.Kind() == config.KindRegistry {
			continue
		}
		reg.providers[key] = reg.createProvider(key, p)
	}
	return reg
}

// Get returns a provider by config key.
func (r *Registry) Get(key string) (FullProvider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if p, ok := r.providers[key]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("provider not found: %s", key)
}

// List returns all registered provider keys.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]string, 0, len(r.providers))
	for key := range r.providers {
		keys = append(keys, key)
	}
	return keys
}

// GetAll returns a copy of all registered providers.
func (r *Registry) GetAll() map[string]FullProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]FullProvider, len(r.providers))
	maps.Copy(result, r.providers)
	return result
}

// Register adds or replaces a provider in the registry from its typed
// config. Used when a provider is enabled at runtime (e.g., after
// verify).
func (r *Registry) Register(key string, p config.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[key] = r.createProvider(key, p)
}

// Unregister removes a provider from the registry.
func (r *Registry) Unregister(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, key)
}

// Has checks if a provider exists in the registry.
func (r *Registry) Has(key string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.providers[key]
	return ok
}

// Count returns the number of registered providers.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}

// Sync reconciles the registry with the current config.
// Uses the same predicate as NewRegistry: providers that are not explicitly
// disabled and not search registries are included. Existing providers are
// kept as-is (not recreated); a server restart is required to pick up
// endpoint/auth changes.
func (r *Registry) Sync(cfg *config.AppsConfig) {
	if cfg == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	all := cfg.AllProviders()
	wanted := make(map[string]bool, len(all))
	for name, p := range all {
		if p.IsExplicitlyDisabled() || p.Kind() == config.KindRegistry {
			continue
		}
		wanted[name] = true
		if _, exists := r.providers[name]; !exists {
			r.providers[name] = r.createProvider(name, p)
		}
	}

	for name := range r.providers {
		if !wanted[name] {
			delete(r.providers, name)
		}
	}
}

// createProvider builds a concrete FullProvider for the given typed
// provider. Kind-scoped type switches happen once here; callers pass a
// config.Provider through, no ServiceConfig involved.
//
// A provider holds no endpoint or credential: each call is given the
// Target its caller resolved from live config, so an edit to either takes
// effect on the next call.
func (r *Registry) createProvider(key string, p config.Provider) FullProvider {
	switch tp := p.(type) {
	case *config.CloudProvider:
		return NewOpenAIProvider(key, 0, string(config.ProtocolOpenAI), r.httpClient)
	case *config.OnDemandProvider:
		if tp.Protocol == config.ProtocolOllama {
			return NewOllamaProvider(r.httpClient)
		}
		return NewOpenAIProvider(key, constants.DefaultVLLMPort, string(tp.Protocol), r.httpClient)
	case *config.ExternalProvider:
		if tp.Protocol == config.ProtocolOllama {
			return NewOllamaProvider(r.httpClient)
		}
		return NewOpenAIProvider(key, constants.DefaultVLLMPort, string(tp.Protocol), r.httpClient)
	default:
		// NewRegistry + Sync filter out SearchRegistry before calling
		// createProvider, so the only way we hit this branch is a new
		// Kind being introduced without a matching case here. Panic
		// rather than silently synthesize an OpenAI provider — the
		// test drop-through was the exact class of bug 5a's strict
		// PopulateProviders rejection aimed to stop.
		panic(fmt.Sprintf("protocol.Registry: unhandled provider kind %q for %q", p.Kind(), key))
	}
}
