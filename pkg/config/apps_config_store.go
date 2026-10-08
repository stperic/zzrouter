package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/stperic/zzrouter/pkg/config/assets"
)

// kindDirKeyFor returns the providers/<subdir>/ name for a Kind.
// Inverse of knownKinds; used by on-disk path helpers.
func kindDirKeyFor(k Kind) string {
	for key, kk := range knownKinds {
		if kk == k {
			return key
		}
	}
	return ""
}

// AppsConfigStore is the single owner of the providers/ directory I/O.
// All reads go through Config(); all writes go through mutation methods
// that atomically update the in-memory config and persist to disk.
// This eliminates the stale-config race where independent load-modify-save
// cycles could overwrite each other's changes.
//
// Subscribers register with OnChange to receive the updated config after
// every successful mutation. This is the one-way contract: if you need to
// react to config changes (e.g., reload a port pool, rebuild a cache), you
// subscribe — you never poll Config() or call a manual sync method.
type AppsConfigStore struct {
	mu        sync.RWMutex
	config    *AppsConfig
	dirPath   string
	listeners []appsListenerEntry
}

// appsListenerEntry pairs the listener's name with its callback so the
// store can tag dispositions in the ReloadReport.
type appsListenerEntry struct {
	name string
	fn   func(*AppsConfig) ReloadDisposition
}

// NewAppsConfigStore loads the providers/ directory at the given path and
// returns a store that owns all subsequent reads and writes.
func NewAppsConfigStore(dirPath string) (*AppsConfigStore, error) {
	cfg, err := LoadAppsConfig(dirPath)
	if err != nil {
		return nil, fmt.Errorf("load apps config: %w", err)
	}
	return &AppsConfigStore{config: cfg, dirPath: dirPath}, nil
}

// NewAppsConfigStoreFromStandardLocations finds the providers/ directory
// via XDG search and returns a store. Primary constructor for server use.
func NewAppsConfigStoreFromStandardLocations() (*AppsConfigStore, error) {
	cm := NewConfigManager("zzrouter")
	dirPath := cm.FindAppsConfigDir()
	if dirPath == "" {
		return nil, fmt.Errorf("providers directory not found in %s", cm.GetNodeConfigDir())
	}
	return NewAppsConfigStore(dirPath)
}

// Config returns the current in-memory config. Callers must NOT mutate
// the returned value directly — use the store's mutation methods instead.
func (s *AppsConfigStore) Config() *AppsConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// DirPath returns the path to the providers/ directory.
func (s *AppsConfigStore) DirPath() string {
	return s.dirPath
}

// ReadProviderConfigBytes returns the raw on-disk config.yaml bytes for
// the named provider. Used as the ETag source for /providers/:n/resolved
// (plan §6.1 — fingerprint the authoritative on-disk state, not the
// in-memory tree). Returns ErrProviderNotFound when absent.
func (s *AppsConfigStore) ReadProviderConfigBytes(name string) ([]byte, error) {
	dir, err := s.providerDir(name)
	if err != nil {
		return nil, err
	}
	fpath := filepath.Join(dir, "config.yaml")
	data, err := os.ReadFile(fpath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fpath, err)
	}
	return data, nil
}

// ReadProviderSchemaBytes returns the sibling schema.yaml bytes for the
// named provider, or (nil, nil) if absent. Used by the coordinator's
// fan-out so workers receive any YAML augmentation alongside config.yaml.
func (s *AppsConfigStore) ReadProviderSchemaBytes(name string) ([]byte, error) {
	dir, err := s.providerDir(name)
	if err != nil {
		return nil, nil //nolint:nilerr // an unknown provider has no schema to send
	}
	fpath := filepath.Join(dir, "schema.yaml")
	data, err := os.ReadFile(fpath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", fpath, err)
	}
	return data, nil
}

// Assets returns the named provider's asset directory. Reads go straight
// to it; writes go through the store so listeners hear of them.
func (s *AppsConfigStore) Assets(name string) (assets.Dir, error) {
	dir, err := s.providerDir(name)
	if err != nil {
		return assets.Dir{}, err
	}
	return assets.NewDir(dir), nil
}

// WriteAsset creates or replaces one of a provider's assets and reports
// whether it was new.
func (s *AppsConfigStore) WriteAsset(provider, name string, data []byte) (created bool, err error) {
	err = s.mutateAssets(provider, func(d assets.Dir) error {
		created, err = d.Write(name, data)
		return err
	})
	return created, err
}

// DeleteAsset removes one of a provider's assets unless inUse names a
// parameter still referring to it. inUse runs under the same lock as
// parameter writes, so no write can name the asset between the check and
// the removal.
func (s *AppsConfigStore) DeleteAsset(provider, name string, inUse func(ServiceConfig) []string) error {
	return s.mutateAssets(provider, func(d assets.Dir) error {
		if err := d.Exists(name); err != nil {
			return err
		}
		if cfg, ok := s.config.LookupApp(provider); ok {
			if paths := inUse(cfg); len(paths) > 0 {
				return &AssetInUseError{Name: name, Paths: paths}
			}
		}
		return d.Delete(name)
	})
}

// AssetInUseError refuses removing an asset parameters still name.
type AssetInUseError struct {
	Name string
	// Paths are the merge-patch paths of the parameters naming it.
	Paths []string
}

func (e *AssetInUseError) Error() string {
	return fmt.Sprintf("asset %q is named by %s", e.Name, strings.Join(e.Paths, ", "))
}

// mutateAssets runs fn on the provider's asset directory under the write
// lock and, when it succeeds, notifies listeners exactly as a config
// mutation does: assets are part of the provider's definition, and the
// peer-sync listener is what carries them to workers.
func (s *AppsConfigStore) mutateAssets(provider string, fn func(assets.Dir) error) error {
	s.mu.Lock()
	dir, err := s.providerDirIn(s.config, provider)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := fn(assets.NewDir(dir)); err != nil {
		s.mu.Unlock()
		return err
	}
	s.unlockAndNotify("asset")
	return nil
}

// providerDir returns providers/<kind>/<name> for a loaded provider.
func (s *AppsConfigStore) providerDir(name string) (string, error) {
	// Mutators edit the provider map in place, so the lookup holds the lock.
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.providerDirIn(s.config, name)
}

// providerDirIn is providerDir against a config the caller already holds.
func (s *AppsConfigStore) providerDirIn(cfg *AppsConfig, name string) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("%w: providers config not loaded", ErrProviderNotFound)
	}
	p, ok := cfg.providers[name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrProviderNotFound, name)
	}
	kindKey := kindDirKeyFor(p.Kind())
	if kindKey == "" {
		return "", fmt.Errorf("unknown kind for provider %q", name)
	}
	return filepath.Join(s.dirPath, kindKey, name), nil
}

// ProviderKind returns the Kind of the named provider, or "" if absent.
// Exposed so the fan-out can key the subdirectory without a second Find.
func (s *AppsConfigStore) ProviderKind(name string) Kind {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.config == nil {
		return ""
	}
	if p, ok := s.config.providers[name]; ok {
		return p.Kind()
	}
	return ""
}

// ProviderFiles is everything the coordinator owns about one provider on
// disk, as peer-sync carries it to a worker.
type ProviderFiles struct {
	Config []byte
	// Schema nil leaves an existing schema.yaml untouched; empty
	// (non-nil) deletes it.
	Schema []byte
	// Assets is the provider's whole asset set: the coordinator is
	// authoritative, so an asset absent here is removed.
	Assets map[string][]byte
}

// WriteProviderFiles atomically replaces one provider's files and
// triggers a full reload so in-memory state tracks the new bytes. Used
// by worker-side peer-sync receivers; the coordinator never calls this
// (it writes via SaveToDir and the asset methods).
//
// It reports whether anything changed. Bytes that already match what is
// on disk are not rewritten and fire no reload, which is what lets the
// coordinator push the whole tree on a schedule to repair drift: a
// worker already in step pays a comparison and nothing else. Without
// that, a periodic push would restart the reload chain - provider
// manager, model registry, model cache - on every node every cycle.
//
// Config is required. An asset set that fails validation is refused
// before anything is written. Errors before the write leave the tree
// unchanged; reload errors after the write return but the bytes have
// already landed (worker-side retry is via re-send from the coord).
func (s *AppsConfigStore) WriteProviderFiles(name string, kind Kind, files ProviderFiles) (changed bool, err error) {
	if name == "" {
		return false, fmt.Errorf("provider name required")
	}
	if len(files.Config) == 0 {
		return false, fmt.Errorf("config.yaml bytes required")
	}
	if err := assets.ValidateSet(files.Assets); err != nil {
		return false, err
	}
	kindKey := kindDirKeyFor(kind)
	if kindKey == "" {
		return false, fmt.Errorf("unknown kind %q", kind)
	}
	s.mu.Lock()
	providerDir := filepath.Join(s.dirPath, kindKey, name)
	// Compared under the same lock as the write, so a concurrent push
	// cannot slip between "matches" and "skip".
	if providerFilesMatch(providerDir, files) {
		s.mu.Unlock()
		return false, nil
	}
	if err := os.MkdirAll(providerDir, 0o755); err != nil {
		s.mu.Unlock()
		return false, fmt.Errorf("create %s: %w", providerDir, err)
	}
	// Assets first: the config may name them, so it never lands ahead of
	// the files it refers to. Strays go first too: pruning after writing
	// would delete an asset renamed only in case on a macOS or Windows
	// worker, where the old and new names are one file.
	if err := assets.NewDir(providerDir).Replace(files.Assets); err != nil {
		s.mu.Unlock()
		return false, fmt.Errorf("write assets of %s: %w", name, err)
	}
	configPath := filepath.Join(providerDir, "config.yaml")
	if err := atomicWriteYAML(configPath, files.Config); err != nil {
		s.mu.Unlock()
		return false, fmt.Errorf("write %s: %w", configPath, err)
	}
	if files.Schema != nil {
		schemaPath := filepath.Join(providerDir, "schema.yaml")
		if len(files.Schema) == 0 {
			_ = os.Remove(schemaPath)
		} else if err := atomicWriteYAML(schemaPath, files.Schema); err != nil {
			s.mu.Unlock()
			return false, fmt.Errorf("write %s: %w", schemaPath, err)
		}
	}

	cfg, err := LoadAppsConfig(s.dirPath)
	if err != nil {
		// Bytes landed on disk but the reload failed — in-memory config
		// is now stale. Next push re-runs the reload; meanwhile the
		// launcher will see old values. Ops must see this.
		slog.Error("provider sync: bytes on-disk but in-memory stale",
			"provider", name, "error", err)
		s.mu.Unlock()
		return true, fmt.Errorf("reload after sync: %w", err)
	}
	s.config = cfg
	s.unlockAndNotify("provider-sync")
	return true, nil
}

// providerFilesMatch reports whether the provider's files on disk already
// equal files. A nil Schema means "leave the sibling alone", so it never
// counts as a difference; the asset set always does, or an asset-only
// change would be skipped as unchanged.
func providerFilesMatch(providerDir string, files ProviderFiles) bool {
	onDisk, err := os.ReadFile(filepath.Join(providerDir, "config.yaml"))
	if err != nil || !bytes.Equal(onDisk, files.Config) {
		return false
	}
	if !assets.NewDir(providerDir).Matches(files.Assets) {
		return false
	}
	if files.Schema == nil {
		return true
	}
	existing, err := os.ReadFile(filepath.Join(providerDir, "schema.yaml"))
	if len(files.Schema) == 0 {
		// Empty-but-not-nil means "delete the sibling": matching means
		// it is already gone.
		return err != nil
	}
	return err == nil && bytes.Equal(existing, files.Schema)
}

// OnChange registers a listener that fires after every successful mutation.
// Listeners run after the write lock is released, so they may safely call
// back into the store. They are invoked in registration order and report
// their result as a ReloadDisposition. See docs/plan_reload_semantics.md.
func (s *AppsConfigStore) OnChange(name string, fn func(*AppsConfig) ReloadDisposition) {
	s.mu.Lock()
	s.listeners = append(s.listeners, appsListenerEntry{name: name, fn: fn})
	s.mu.Unlock()
}

// Reload re-reads the providers/ directory from disk, replacing the
// in-memory config, and notifies listeners. Returns the aggregated
// ReloadReport so callers can surface per-listener dispositions.
func (s *AppsConfigStore) Reload() (*ReloadReport, error) {
	s.mu.Lock()
	cfg, err := LoadAppsConfig(s.dirPath)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("reload apps config: %w", err)
	}
	s.config = cfg
	return s.unlockAndNotify("disk-reload"), nil
}

// save persists the current in-memory config to disk. Must be called with mu held.
func (s *AppsConfigStore) save() error {
	return s.config.SaveToDir(s.dirPath)
}

// mutate runs the closure under the write lock, persists on success, and
// notifies listeners after the lock is released. Closures signal whether
// anything actually changed: changed=false skips save and notification
// (idempotent no-op). This is the single entry point for all mutation
// methods; callers must NOT hold the lock when calling this.
func (s *AppsConfigStore) mutate(fn func() (changed bool, err error)) error {
	s.mu.Lock()
	changed, err := fn()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if !changed {
		s.mu.Unlock()
		return nil
	}
	if err := s.save(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.unlockAndNotify("mutation")
	return nil
}

// unlockAndNotify releases the write lock and tells listeners about the
// config now held. They run outside the lock so they may call back in.
func (s *AppsConfigStore) unlockAndNotify(source string) *ReloadReport {
	cfg := s.config
	listeners := s.snapshotListenersLocked()
	s.mu.Unlock()
	return s.notify(cfg, listeners, source)
}

// snapshotListenersLocked copies the listener slice under the write lock so
// the caller can invoke listeners after releasing the lock. Callers must hold
// s.mu at least for read.
func (s *AppsConfigStore) snapshotListenersLocked() []appsListenerEntry {
	if len(s.listeners) == 0 {
		return nil
	}
	out := make([]appsListenerEntry, len(s.listeners))
	copy(out, s.listeners)
	return out
}

// notify invokes listeners without holding the lock, aggregates their
// dispositions, and logs a summary. A panicking listener is recorded as
// DispositionRejected and the chain continues so downstream reloaders
// still reach the new config.
func (s *AppsConfigStore) notify(cfg *AppsConfig, listeners []appsListenerEntry, source string) *ReloadReport {
	report := &ReloadReport{}
	for i, l := range listeners {
		report.Add(l.name, s.invokeListener(i, l, cfg))
	}
	report.LogSummary(source)
	return report
}

// invokeListener calls a single listener under a panic recovery and
// returns its disposition (Rejected if it panicked).
func (s *AppsConfigStore) invokeListener(idx int, l appsListenerEntry, cfg *AppsConfig) (disp ReloadDisposition) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("apps config listener panicked; continuing chain",
				"listener", l.name,
				"index", idx,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			disp = DispositionRejected
		}
	}()
	return l.fn(cfg)
}

// ============================================================================
// Runtime Provider Instantiation
// ============================================================================

// AddProviderInstance inserts a typed provider at runtime and persists it
// atomically. Used by POST /zzrouter/v1/providers/instances to create
// user-authored entries such as ollama connect instances — the store's
// own code never constructs providers, so the typed Provider argument
// stays narrow.
//
// The provider goes through AddApp's ServiceConfig conversion path so the
// validation and kind-dispatch logic are shared with yaml load. Fails on
// name collision across any kind (AddApp enforces cross-kind uniqueness).
// Listeners fire after the new file is persisted to the providers/
// directory by SaveToDir.
func (s *AppsConfigStore) AddProviderInstance(name string, p Provider) error {
	return s.mutate(func() (bool, error) {
		sc := providerToServiceConfig(p)
		if err := s.config.AddApp(name, sc); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ============================================================================
// Provider Enabled State
// ============================================================================

// SetProviderEnabled sets enabled: true/false for a provider and persists.
//
// Returns ErrProviderNotFound (wrapped) if the provider is absent — callers
// should use errors.Is to map to 404 at the HTTP edge.
func (s *AppsConfigStore) SetProviderEnabled(name string, enabled bool) error {
	return s.mutate(func() (bool, error) {
		svc, exists := s.config.LookupApp(name)
		if !exists {
			return false, fmt.Errorf("%w: %q", ErrProviderNotFound, name)
		}
		if svc.IsEnabled() == enabled {
			return false, nil
		}
		if err := s.config.UpdateApp(name, func(sc *ServiceConfig) error {
			sc.SetEnabled(enabled)
			return nil
		}); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ============================================================================
// Provider Pinned Version
// ============================================================================

// SetProviderPinnedVersion sets the install version pin for a provider and
// persists. An empty version clears the pin, which restores the installer's
// "resolve latest upstream release" behaviour.
//
// The pin is what install/upgrade resolve against when the caller supplies no
// explicit version, so writing it here is the durable counterpart to a
// one-shot per-request version override.
//
// Returns ErrProviderNotFound (wrapped) if the provider is absent.
func (s *AppsConfigStore) SetProviderPinnedVersion(name, version string) error {
	return s.mutate(func() (bool, error) {
		svc, exists := s.config.LookupApp(name)
		if !exists {
			return false, fmt.Errorf("%w: %q", ErrProviderNotFound, name)
		}
		if svc.PinnedVersion == version {
			return false, nil
		}
		if err := s.config.UpdateApp(name, func(sc *ServiceConfig) error {
			sc.PinnedVersion = version
			return nil
		}); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ============================================================================
// Cloud Model Management
// ============================================================================

// AddCloudModel adds a model ID to a provider's capabilities and persists.
func (s *AppsConfigStore) AddCloudModel(providerName, modelID string) error {
	return s.mutate(func() (bool, error) {
		provider, exists := s.config.LookupApp(providerName)
		if !exists {
			return false, fmt.Errorf("provider %q not found", providerName)
		}
		if !provider.IsCloudProvider() {
			hasCredentials := provider.Runtime != nil && provider.Runtime.API != nil && provider.Runtime.API.HasCredentials()
			if !hasCredentials {
				return false, fmt.Errorf("provider %q requires an API key to register cloud models", providerName)
			}
		}
		if provider.Capabilities != nil {
			for _, m := range provider.Capabilities.Models {
				if strings.EqualFold(m, modelID) {
					return false, nil
				}
			}
		}
		if err := s.config.UpdateApp(providerName, func(sc *ServiceConfig) error {
			if sc.Capabilities == nil {
				sc.Capabilities = &AppCapabilities{}
			}
			models := make([]string, len(sc.Capabilities.Models)+1)
			copy(models, sc.Capabilities.Models)
			models[len(models)-1] = modelID
			sc.Capabilities.Models = models
			return nil
		}); err != nil {
			return false, err
		}
		return true, nil
	})
}

// RemoveCloudModels removes models from providers and persists (single lock + save + notify).
func (s *AppsConfigStore) RemoveCloudModels(modelsByProvider map[string][]string) error {
	if len(modelsByProvider) == 0 {
		return nil
	}
	return s.mutate(func() (bool, error) {
		changed := false
		for providerName, modelIDs := range modelsByProvider {
			provider, exists := s.config.LookupApp(providerName)
			if !exists || provider.Capabilities == nil {
				continue
			}
			removeSet := make(map[string]bool, len(modelIDs))
			for _, id := range modelIDs {
				removeSet[id] = true
			}
			var kept []string
			for _, m := range provider.Capabilities.Models {
				if !removeSet[m] {
					kept = append(kept, m)
				}
			}
			if len(kept) == len(provider.Capabilities.Models) {
				continue
			}
			// `kept` was computed off the synthesized ServiceConfig view; re-apply
			// it through UpdateApp so the typed Provider in c.providers is the
			// one that gets the post-removal slice.
			if err := s.config.UpdateApp(providerName, func(sc *ServiceConfig) error {
				sc.Capabilities.Models = kept
				return nil
			}); err != nil {
				return changed, err
			}
			changed = true
		}
		return changed, nil
	})
}

// ============================================================================
// Parameter CRUD — App Level
// ============================================================================

// ReplaceAppParameters replaces app-level parameters/environment and persists.
func (s *AppsConfigStore) ReplaceAppParameters(provider string, params, env map[string]string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.ReplaceAppParameters(provider, params, env); err != nil {
			return false, err
		}
		return true, nil
	})
}

// SetAppParameter sets a single app-level parameter and persists.
func (s *AppsConfigStore) SetAppParameter(provider, key, value string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.SetAppParameter(provider, key, value); err != nil {
			return false, err
		}
		return true, nil
	})
}

// DeleteAppParameter removes a single app-level parameter and persists.
func (s *AppsConfigStore) DeleteAppParameter(provider, key string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.DeleteAppParameter(provider, key); err != nil {
			return false, err
		}
		return true, nil
	})
}

// SetAppEnvironment sets a single app-level env var and persists.
func (s *AppsConfigStore) SetAppEnvironment(provider, key, value string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.SetAppEnvironment(provider, key, value); err != nil {
			return false, err
		}
		return true, nil
	})
}

// DeleteAppEnvironment removes a single app-level env var and persists.
func (s *AppsConfigStore) DeleteAppEnvironment(provider, key string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.DeleteAppEnvironment(provider, key); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ============================================================================
// Parameter CRUD — Model Level
// ============================================================================

// ReplaceModelParameters replaces model-level parameters/environment and persists.
func (s *AppsConfigStore) ReplaceModelParameters(provider, model string, params, env map[string]string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.ReplaceModelParameters(provider, model, params, env); err != nil {
			return false, err
		}
		return true, nil
	})
}

// DeleteModelParameter removes a single model-level parameter and persists.
func (s *AppsConfigStore) DeleteModelParameter(provider, model, key string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.DeleteModelParameter(provider, model, key); err != nil {
			return false, err
		}
		return true, nil
	})
}

// DeleteModelEnvironment removes a single model-level env var and persists.
func (s *AppsConfigStore) DeleteModelEnvironment(provider, model, key string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.DeleteModelEnvironment(provider, model, key); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ============================================================================
// Parameter CRUD — Node Level
// ============================================================================

// ReplaceNodeParameters replaces node-level parameters/environment and persists.
func (s *AppsConfigStore) ReplaceNodeParameters(provider, node string, params, env map[string]string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.ReplaceNodeParameters(provider, node, params, env); err != nil {
			return false, err
		}
		return true, nil
	})
}

// DeleteNodeParameter removes a single node-level parameter and persists.
func (s *AppsConfigStore) DeleteNodeParameter(provider, node, key string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.DeleteNodeParameter(provider, node, key); err != nil {
			return false, err
		}
		return true, nil
	})
}

// DeleteNodeEnvironment removes a single node-level env var and persists.
func (s *AppsConfigStore) DeleteNodeEnvironment(provider, node, key string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.DeleteNodeEnvironment(provider, node, key); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ApplyParameterPatch runs a caller-supplied mutator over the provider's
// ServiceConfig under a single lock+save+notify cycle. Used by the
// RFC 7396 Merge-Patch handler so the whole patch is atomic: either
// every leaf lands or none do. Returns ErrProviderNotFound when provider
// is absent.
func (s *AppsConfigStore) ApplyParameterPatch(provider string, mutate func(*ServiceConfig) error) error {
	return s.mutate(func() (bool, error) {
		if _, exists := s.config.LookupApp(provider); !exists {
			return false, fmt.Errorf("%w: %q", ErrProviderNotFound, provider)
		}
		if err := s.config.UpdateApp(provider, mutate); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ============================================================================
// Parameter CRUD — Node × Model (Tier 3)
// ============================================================================

// SetNodeModelParameter sets a single (node, model) parameter and persists.
func (s *AppsConfigStore) SetNodeModelParameter(provider, node, model, key, value string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.SetNodeModelParameter(provider, node, model, key, value); err != nil {
			return false, err
		}
		return true, nil
	})
}

// DeleteNodeModelParameter removes a single (node, model) parameter and persists.
func (s *AppsConfigStore) DeleteNodeModelParameter(provider, node, model, key string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.DeleteNodeModelParameter(provider, node, model, key); err != nil {
			return false, err
		}
		return true, nil
	})
}

// SetNodeModelEnvironment sets a single (node, model) env var and persists.
func (s *AppsConfigStore) SetNodeModelEnvironment(provider, node, model, key, value string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.SetNodeModelEnvironment(provider, node, model, key, value); err != nil {
			return false, err
		}
		return true, nil
	})
}

// DeleteNodeModelEnvironment removes a single (node, model) env var and persists.
func (s *AppsConfigStore) DeleteNodeModelEnvironment(provider, node, model, key string) error {
	return s.mutate(func() (bool, error) {
		if err := s.config.DeleteNodeModelEnvironment(provider, node, model, key); err != nil {
			return false, err
		}
		return true, nil
	})
}
