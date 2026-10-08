package keys

import (
	"fmt"
	"maps"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/utils"
	"gopkg.in/yaml.v3"
)

// keysFile is the on-disk format for keys.yaml.
type keysFile struct {
	Version string                     `yaml:"version"`
	Keys    map[string]*virtualKeyYAML `yaml:"keys,omitempty"`
}

// virtualKeyYAML is the on-disk representation with all fields.
type virtualKeyYAML struct {
	UUID      string `yaml:"uuid,omitempty"`
	Name      string `yaml:"name"`
	HashedKey string `yaml:"hashed_key,omitempty"`
	KeyEnv    string `yaml:"key_env,omitempty"`

	TeamID   string `yaml:"team_id"`
	TeamRole string `yaml:"team_role"`

	// AllowedModels is parsed only so we can reject it — per-key model
	// access does not exist. If yaml sets this field we refuse the file
	// rather than silently dropping it, since the key would inherit its
	// team's (likely wider) allow-list.
	AllowedModels []string `yaml:"allowed_models,omitempty"`

	Role        string  `yaml:"role"`
	Suspended   bool    `yaml:"suspended,omitempty"`
	SuspendedAt *string `yaml:"suspended_at,omitempty"`
	SuspendedBy string  `yaml:"suspended_by,omitempty"`

	RPMLimit            int     `yaml:"rpm_limit,omitempty"`
	TPMLimit            int     `yaml:"tpm_limit,omitempty"`
	MaxParallelRequests int     `yaml:"max_parallel_requests,omitempty"`
	SpendLimit          float64 `yaml:"spend_limit,omitempty"`
	ResetPeriod         string  `yaml:"reset_period,omitempty"`
	DefaultMaxTokens    int     `yaml:"default_max_tokens,omitempty"`

	ExpiresAt *string           `yaml:"expires_at,omitempty"`
	CreatedAt *string           `yaml:"created_at,omitempty"`
	UpdatedAt *string           `yaml:"updated_at,omitempty"`
	CreatedBy string            `yaml:"created_by,omitempty"`
	UpdatedBy string            `yaml:"updated_by,omitempty"`
	Metadata  map[string]string `yaml:"metadata,omitempty"`
}

// FileKeyStore implements KeyStore backed by keys.yaml.
type FileKeyStore struct {
	mu   sync.RWMutex
	keys map[string]*VirtualKey
	path string

	// listeners fire (outside any store lock) after every successful
	// mutation. Registered via OnChange. Notifying after unlock avoids
	// the listener calling back into the store and deadlocking.
	listenersMu sync.Mutex
	listeners   []func()
}

// OnChange registers a listener invoked after every successful mutation.
// Listeners run after the store lock is released so they may safely call
// back into the store. Invoked in registration order.
func (s *FileKeyStore) OnChange(fn func()) {
	s.listenersMu.Lock()
	s.listeners = append(s.listeners, fn)
	s.listenersMu.Unlock()
}

// notifyListeners invokes every registered listener. Callers MUST release
// the store lock before calling this — the listener is free to read from
// the store and would otherwise deadlock on the RWMutex.
func (s *FileKeyStore) notifyListeners() {
	s.listenersMu.Lock()
	listeners := make([]func(), len(s.listeners))
	copy(listeners, s.listeners)
	s.listenersMu.Unlock()
	for _, fn := range listeners {
		fn()
	}
}

// NewFileKeyStore creates a key store that reads/writes to the given file path.
func NewFileKeyStore(path string) *FileKeyStore {
	return &FileKeyStore{
		keys: make(map[string]*VirtualKey),
		path: path,
	}
}

// Load reads virtual keys from keys.yaml.
func (s *FileKeyStore) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("failed to read keys config: %w", err)
	}
	return s.loadFromBytes(data)
}

func (s *FileKeyStore) loadFromBytes(data []byte) error {
	var cfg keysFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse keys config: %w", err)
	}

	keys := make(map[string]*VirtualKey, len(cfg.Keys))
	for id, yk := range cfg.Keys {
		vk, err := fromYAML(id, yk)
		if err != nil {
			return fmt.Errorf("key %q: %w", id, err)
		}
		keys[id] = vk
	}

	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()

	s.notifyListeners()
	return nil
}

// Get returns a virtual key by ID, or nil if not found.
func (s *FileKeyStore) Get(id string) *VirtualKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keys[id]
}

// List returns all virtual keys (copies to prevent mutation).
func (s *FileKeyStore) List() map[string]*VirtualKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]*VirtualKey, len(s.keys))
	maps.Copy(result, s.keys)
	return result
}

// ListByTeam returns all keys whose TeamID matches. Empty map on no matches.
// Replaces the old "iterate + filter" pattern with a single store-aware call
// so the future database backend can serve this as an indexed query.
func (s *FileKeyStore) ListByTeam(teamID string) map[string]*VirtualKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]*VirtualKey)
	if teamID == "" {
		return result
	}
	for id, vk := range s.keys {
		if vk.TeamID == teamID {
			result[id] = vk
		}
	}
	return result
}

// ValidateRawKey checks a raw API key against all stored keys.
// For env-var keys, compares directly. For hashed keys, uses Argon2id verify.
func (s *FileKeyStore) ValidateRawKey(rawKey string) (string, *VirtualKey) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for id, vk := range s.keys {
		// Env-var keys: resolve and compare
		if vk.KeyEnv != "" {
			envVal := os.Getenv(vk.KeyEnv)
			if envVal != "" && envVal == rawKey {
				return id, vk
			}
			continue
		}

		// Hashed keys: Argon2id verify
		if vk.HashedKey != "" && VerifyKey(rawKey, vk.HashedKey) {
			return id, vk
		}
	}

	return "", nil
}

// Create adds a new virtual key. Returns the generated raw key.
// Caller must set TeamID and TeamRole on the key before calling — the store
// does not auto-assign teams. That's the service layer's responsibility
// (KeysService auto-creates a personal team when the caller omits team_id).
func (s *FileKeyStore) Create(id string, key *VirtualKey) (rawKey string, err error) {
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		if err == nil {
			s.notifyListeners()
		}
	}()

	if _, exists := s.keys[id]; exists {
		return "", fmt.Errorf("key %q: %w", id, ErrKeyExists)
	}
	if key.TeamID == "" {
		return "", fmt.Errorf("key %q: team_id is required", id)
	}

	rawKey, err = GenerateRawKey()
	if err != nil {
		return "", err
	}

	hash, err := HashKey(rawKey)
	if err != nil {
		return "", err
	}

	key.ID = id
	key.HashedKey = hash
	key.KeyEnv = "" // Ensure env-var mode is cleared for created keys
	if key.UUID == "" {
		key.UUID = uuid.New().String()
	}
	now := utils.NowUTC()
	key.CreatedAt = now
	key.UpdatedAt = now
	s.keys[id] = key

	return rawKey, nil
}

// Update modifies an existing virtual key's mutable fields. TeamID and
// TeamRole are preserved from the existing record — callers cannot move a
// key between teams via PATCH. Moving a key = delete and re-create.
func (s *FileKeyStore) Update(id string, key *VirtualKey) (err error) {
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		if err == nil {
			s.notifyListeners()
		}
	}()

	existing, ok := s.keys[id]
	if !ok {
		return fmt.Errorf("key %q: %w", id, ErrKeyNotFound)
	}

	// Preserve immutable fields
	key.ID = id
	key.UUID = existing.UUID
	key.HashedKey = existing.HashedKey
	key.KeyEnv = existing.KeyEnv
	key.TeamID = existing.TeamID
	key.TeamRole = existing.TeamRole
	key.CreatedAt = existing.CreatedAt
	key.CreatedBy = existing.CreatedBy
	key.UpdatedAt = utils.NowUTC()
	s.keys[id] = key

	return nil
}

// Delete removes a virtual key.
func (s *FileKeyStore) Delete(id string) (err error) {
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		if err == nil {
			s.notifyListeners()
		}
	}()

	if _, ok := s.keys[id]; !ok {
		return fmt.Errorf("key %q: %w", id, ErrKeyNotFound)
	}

	delete(s.keys, id)
	return nil
}

// RotateKey generates a new raw key for an existing key ID.
func (s *FileKeyStore) RotateKey(id string) (rawKey string, err error) {
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		if err == nil {
			s.notifyListeners()
		}
	}()

	vk, ok := s.keys[id]
	if !ok {
		return "", fmt.Errorf("key %q: %w", id, ErrKeyNotFound)
	}

	rawKey, err = GenerateRawKey()
	if err != nil {
		return "", err
	}

	hash, err := HashKey(rawKey)
	if err != nil {
		return "", err
	}

	vk.HashedKey = hash
	vk.KeyEnv = "" // Clear env-var on rotation
	return rawKey, nil
}

// Reload re-reads keys from disk.
func (s *FileKeyStore) Reload() error {
	return s.Load()
}

// Save persists the current state to disk using atomic temp-file + rename.
func (s *FileKeyStore) Save() error {
	s.mu.RLock()
	cfg := keysFile{
		Version: "1",
		Keys:    make(map[string]*virtualKeyYAML, len(s.keys)),
	}
	for id, vk := range s.keys {
		cfg.Keys[id] = toYAML(vk)
	}
	s.mu.RUnlock()

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal keys: %w", err)
	}

	return utils.AtomicWriteFile(s.path, data, 0o600)
}

// fromYAML converts the on-disk format to VirtualKey.
func fromYAML(id string, yk *virtualKeyYAML) (*VirtualKey, error) {
	if yk.Name == "" {
		return nil, fmt.Errorf("missing name")
	}
	if yk.Role == "" {
		yk.Role = "user"
	}
	if yk.HashedKey == "" && yk.KeyEnv == "" {
		return nil, fmt.Errorf("must have hashed_key or key_env")
	}
	if yk.TeamID == "" {
		return nil, fmt.Errorf("missing team_id")
	}
	if yk.TeamRole == "" {
		return nil, fmt.Errorf("missing team_role")
	}
	if len(yk.AllowedModels) > 0 {
		return nil, fmt.Errorf("allowed_models on keys is not supported; " +
			"set model access on the team and remove this field from the key")
	}

	vk := &VirtualKey{
		UUID:        yk.UUID,
		ID:          id,
		Name:        yk.Name,
		HashedKey:   yk.HashedKey,
		KeyEnv:      yk.KeyEnv,
		TeamID:      yk.TeamID,
		TeamRole:    yk.TeamRole,
		Role:        yk.Role,
		Suspended:   yk.Suspended,
		SuspendedBy: yk.SuspendedBy,
		Metadata:    yk.Metadata,
		CreatedBy:   yk.CreatedBy,
		UpdatedBy:   yk.UpdatedBy,
	}

	vk.QuotaConfig.RPMLimit = yk.RPMLimit
	vk.QuotaConfig.TPMLimit = yk.TPMLimit
	vk.QuotaConfig.MaxParallelRequests = yk.MaxParallelRequests
	vk.QuotaConfig.SpendLimit = yk.SpendLimit
	vk.QuotaConfig.ResetPeriod = yk.ResetPeriod
	vk.QuotaConfig.DefaultMaxTokens = yk.DefaultMaxTokens

	if yk.ExpiresAt != nil {
		t, err := parseTime(*yk.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("invalid expires_at: %w", err)
		}
		vk.ExpiresAt = &t
	}
	if yk.SuspendedAt != nil {
		t, err := parseTime(*yk.SuspendedAt)
		if err != nil {
			return nil, fmt.Errorf("invalid suspended_at: %w", err)
		}
		vk.SuspendedAt = &t
	}
	if yk.CreatedAt != nil {
		t, err := parseTime(*yk.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("invalid created_at: %w", err)
		}
		vk.CreatedAt = t
	}
	if yk.UpdatedAt != nil {
		t, err := parseTime(*yk.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("invalid updated_at: %w", err)
		}
		vk.UpdatedAt = t
	}

	return vk, nil
}

// toYAML converts VirtualKey to on-disk format.
func toYAML(vk *VirtualKey) *virtualKeyYAML {
	yk := &virtualKeyYAML{
		UUID:                vk.UUID,
		Name:                vk.Name,
		HashedKey:           vk.HashedKey,
		KeyEnv:              vk.KeyEnv,
		TeamID:              vk.TeamID,
		TeamRole:            vk.TeamRole,
		Role:                vk.Role,
		Suspended:           vk.Suspended,
		SuspendedBy:         vk.SuspendedBy,
		RPMLimit:            vk.QuotaConfig.RPMLimit,
		TPMLimit:            vk.QuotaConfig.TPMLimit,
		MaxParallelRequests: vk.QuotaConfig.MaxParallelRequests,
		SpendLimit:          vk.QuotaConfig.SpendLimit,
		ResetPeriod:         vk.QuotaConfig.ResetPeriod,
		DefaultMaxTokens:    vk.QuotaConfig.DefaultMaxTokens,
		Metadata:            vk.Metadata,
		CreatedBy:           vk.CreatedBy,
		UpdatedBy:           vk.UpdatedBy,
	}

	if vk.ExpiresAt != nil {
		s := formatTime(*vk.ExpiresAt)
		yk.ExpiresAt = &s
	}
	if vk.SuspendedAt != nil {
		s := formatTime(*vk.SuspendedAt)
		yk.SuspendedAt = &s
	}
	if !vk.CreatedAt.IsZero() {
		s := formatTime(vk.CreatedAt)
		yk.CreatedAt = &s
	}
	if !vk.UpdatedAt.IsZero() {
		s := formatTime(vk.UpdatedAt)
		yk.UpdatedAt = &s
	}

	return yk
}

func formatTime(t time.Time) string {
	return t.Format(time.RFC3339)
}

// parseTime parses an RFC3339 timestamp string.
func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}
