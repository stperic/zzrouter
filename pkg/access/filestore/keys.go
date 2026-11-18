package filestore

import (
	"fmt"
	"time"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"gopkg.in/yaml.v3"
)

// keysFile is the on-disk format for keys.yaml.
type keysFile struct {
	Version string                     `yaml:"version"`
	Keys    map[string]*virtualKeyYAML `yaml:"keys,omitempty"`
}

// virtualKeyYAML is the on-disk representation with all fields.
type virtualKeyYAML struct {
	UUID        string `yaml:"uuid,omitempty"`
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	HashedKey   string `yaml:"hashed_key,omitempty"`
	KeyEnv      string `yaml:"key_env,omitempty"`

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

// DecodeKeys shares the ordinary loader's validation without publishing state.
func DecodeKeys(data []byte) (map[string]*keys.VirtualKey, error) {
	var cfg keysFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse keys config: %w", err)
	}

	keys := make(map[string]*keys.VirtualKey, len(cfg.Keys))
	for id, yk := range cfg.Keys {
		vk, err := fromYAML(id, yk)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", id, err)
		}
		keys[id] = vk
	}

	return keys, nil
}

func encodeKeys(records map[string]*keys.VirtualKey) ([]byte, error) {
	cfg := keysFile{Version: "1", Keys: make(map[string]*virtualKeyYAML, len(records))}
	for id, vk := range records {
		cfg.Keys[id] = toYAML(vk)
	}
	return yaml.Marshal(&cfg)
}

// fromYAML converts the on-disk format to VirtualKey.
func fromYAML(id string, yk *virtualKeyYAML) (*keys.VirtualKey, error) {
	if yk == nil {
		return nil, fmt.Errorf("key must be an object")
	}
	if yk.Role == "" {
		yk.Role = "user"
	}
	if len(yk.AllowedModels) > 0 {
		return nil, fmt.Errorf("allowed_models on keys is not supported; " +
			"set model access on the team and remove this field from the key")
	}

	vk := &keys.VirtualKey{
		UUID:        yk.UUID,
		ID:          id,
		Name:        yk.Name,
		Description: yk.Description,
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

	if err := vk.Validate(); err != nil {
		return nil, err
	}
	return vk, nil
}

// toYAML converts VirtualKey to on-disk format.
func toYAML(vk *keys.VirtualKey) *virtualKeyYAML {
	yk := &virtualKeyYAML{
		UUID:                vk.UUID,
		Name:                vk.Name,
		Description:         vk.Description,
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
