package config

import (
	"fmt"
	"sync"
)

// ClientConfigStore is the single owner of cli.yaml I/O.
// All reads go through Config(); all writes go through mutation methods
// that atomically update in-memory config and persist to disk.
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

// save persists the current in-memory config to disk. Must be called with mu held.
func (s *ClientConfigStore) save() error {
	return s.cm.SaveClientConfig(s.cfg)
}

// mutate runs the closure under the write lock and persists on success.
func (s *ClientConfigStore) mutate(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(); err != nil {
		return err
	}
	return s.save()
}

// ============================================================================
// Node Connection
// ============================================================================

// SetNodeConnection updates the server connection details and persists.
func (s *ClientConfigStore) SetNodeConnection(address string, port int, secure bool) error {
	return s.mutate(func() error {
		s.cfg.Node.Address = address
		s.cfg.Node.Port = port
		s.cfg.Node.Secure = secure
		return nil
	})
}

// SetNodeKey updates the API key reference and persists.
func (s *ClientConfigStore) SetNodeKey(key string) error {
	return s.mutate(func() error {
		s.cfg.Node.Key = key
		return nil
	})
}

// Disconnect clears the node connection (keeps key) and persists.
func (s *ClientConfigStore) Disconnect() error {
	return s.mutate(func() error {
		s.cfg.Node.Address = ""
		s.cfg.Node.Port = 0
		s.cfg.Node.Secure = false
		return nil
	})
}

// ============================================================================
// Preferences
// ============================================================================

// SetTheme sets the UI theme and persists.
func (s *ClientConfigStore) SetTheme(theme string) error {
	return s.mutate(func() error {
		s.cfg.Preferences.Theme = theme
		return nil
	})
}

// SetChatSettings updates chat session preferences and persists.
func (s *ClientConfigStore) SetChatSettings(settings ChatSettings) error {
	return s.mutate(func() error {
		s.cfg.Preferences.Chat = settings
		return nil
	})
}

// SetSearchPrefs updates search registry preferences and persists.
func (s *ClientConfigStore) SetSearchPrefs(registry, sort string, tags []string) error {
	return s.mutate(func() error {
		s.cfg.Preferences.Search.DefaultRegistry = registry
		if s.cfg.Preferences.Search.RegistryPrefs == nil {
			s.cfg.Preferences.Search.RegistryPrefs = make(map[string]SearchRegistryPrefs)
		}
		s.cfg.Preferences.Search.RegistryPrefs[registry] = SearchRegistryPrefs{
			Sort: sort,
			Tags: tags,
		}
		return nil
	})
}

// SetModelTags updates the model tag filters and persists.
func (s *ClientConfigStore) SetModelTags(tags []TagFilter) error {
	return s.mutate(func() error {
		s.cfg.Preferences.Search.ModelTags = tags
		return nil
	})
}
