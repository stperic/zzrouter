package config

import (
	"fmt"
	"slices"
	"sync"
)

// ClientConfigStore owns immutable client snapshots and field mutations.
// Its ConfigManager serializes file access across stores and processes.
//
// Although CLI commands are typically sequential, this store prevents
// the load-modify-save race when multiple commands run in quick succession
// (e.g., search UI saving prefs while chat session saves settings).
type ClientConfigStore struct {
	mu  sync.RWMutex
	cm  *ConfigManager
	cfg *ClientConfig
}

// NewClientConfigStore loads cli.yaml via the given ConfigManager and returns
// a store that owns all subsequent reads and writes.
func NewClientConfigStore(cm *ConfigManager) (*ClientConfigStore, error) {
	cfg, err := cm.LoadClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load client config: %w", err)
	}
	return &ClientConfigStore{cm: cm, cfg: cfg}, nil
}

// Config returns the current in-memory config. Callers must NOT mutate
// the returned value directly — use the store's mutation methods instead.
func (s *ClientConfigStore) Config() *ClientConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// mutate applies one field change to the latest persisted config. Existing
// snapshots returned by Config remain immutable, including on a failed save.
func (s *ClientConfigStore) mutate(fn func(*ClientConfig)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, err := s.cm.lockClientConfig()
	if err != nil {
		return err
	}
	defer func() { _ = owner.Close() }()
	next, err := s.cm.loadClientConfig()
	if err != nil {
		return fmt.Errorf("reload client config: %w", err)
	}
	fn(next)
	if err := s.cm.saveClientConfig(next); err != nil {
		return err
	}
	s.cfg = next
	return nil
}

// ============================================================================
// Node Connection
// ============================================================================

// SetNodeConnection updates the server connection details and persists.
func (s *ClientConfigStore) SetNodeConnection(address string, port int, secure bool) error {
	return s.mutate(func(cfg *ClientConfig) {
		cfg.Node.Address = address
		cfg.Node.Port = port
		cfg.Node.Secure = secure
	})
}

// SetNodeKey updates the API key reference and persists.
func (s *ClientConfigStore) SetNodeKey(key string) error {
	return s.mutate(func(cfg *ClientConfig) {
		cfg.Node.Key = key
	})
}

// Disconnect clears the node connection (keeps key) and persists.
func (s *ClientConfigStore) Disconnect() error {
	return s.mutate(func(cfg *ClientConfig) {
		cfg.Node.Address = ""
		cfg.Node.Port = 0
		cfg.Node.Secure = false
	})
}

// ============================================================================
// Preferences
// ============================================================================

// SetTheme sets the UI theme and persists.
func (s *ClientConfigStore) SetTheme(theme string) error {
	return s.mutate(func(cfg *ClientConfig) {
		cfg.Preferences.Theme = theme
	})
}

// SetChatSettings updates chat session preferences and persists.
func (s *ClientConfigStore) SetChatSettings(settings ChatSettings) error {
	return s.mutate(func(cfg *ClientConfig) {
		cfg.Preferences.Chat = settings
	})
}

// SetSearchPrefs updates search registry preferences and persists.
func (s *ClientConfigStore) SetSearchPrefs(registry, sort string, tags []string) error {
	return s.mutate(func(cfg *ClientConfig) {
		cfg.Preferences.Search.DefaultRegistry = registry
		if cfg.Preferences.Search.RegistryPrefs == nil {
			cfg.Preferences.Search.RegistryPrefs = make(map[string]SearchRegistryPrefs)
		}
		cfg.Preferences.Search.RegistryPrefs[registry] = SearchRegistryPrefs{
			Sort: sort,
			Tags: slices.Clone(tags),
		}
	})
}

// SetModelTags updates the model tag filters and persists.
func (s *ClientConfigStore) SetModelTags(tags []TagFilter) error {
	return s.mutate(func(cfg *ClientConfig) {
		cfg.Preferences.Search.ModelTags = slices.Clone(tags)
		for i := range cfg.Preferences.Search.ModelTags {
			cfg.Preferences.Search.ModelTags[i].Tags = slices.Clone(tags[i].Tags)
		}
	})
}
