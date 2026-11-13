package teams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProviderAccess restricts provider identities independently of model names.
// The zero value preserves the unrestricted policy of existing teams.
type ProviderAccess struct {
	Mode      string   `json:"mode" yaml:"mode"`
	Providers []string `json:"providers,omitempty" yaml:"providers,omitempty"`
}

// IsZero matches omission in PATCH and preserves allocated-empty client slices.
func (p ProviderAccess) IsZero() bool { return p.Mode == "" && len(p.Providers) == 0 }

// Restricted reports whether a policy narrows provider access.
func (p ProviderAccess) Restricted() bool { return p.Mode != "" && p.Mode != "all" }

// Allows compares exact provider keys, refusing unknown targets when restricted.
func (p ProviderAccess) Allows(provider string) bool {
	return !p.Restricted() || p.Mode == "selected" && provider != "" && slices.Contains(p.Providers, provider)
}

// Clone keeps policy snapshots independent of request and stored slices.
func (p ProviderAccess) Clone() ProviderAccess {
	p.Providers = slices.Clone(p.Providers)
	return p
}

// Equal compares effective policies without treating provider order as a change.
func (p ProviderAccess) Equal(other ProviderAccess) bool {
	if !p.Restricted() || !other.Restricted() {
		return p.Restricted() == other.Restricted()
	}
	if p.Mode != other.Mode || len(p.Providers) != len(other.Providers) {
		return false
	}
	for _, provider := range p.Providers {
		if !slices.Contains(other.Providers, provider) {
			return false
		}
	}
	return true
}

// Validate rejects ambiguous policies rather than widening access.
func (p ProviderAccess) Validate() error {
	switch p.Mode {
	case "", "all", "none":
		if len(p.Providers) != 0 {
			return fmt.Errorf("provider_access.providers requires selected mode")
		}
	case "selected":
		if len(p.Providers) == 0 {
			return fmt.Errorf("select at least one provider, or use none")
		}
		seen := make(map[string]bool, len(p.Providers))
		for _, key := range p.Providers {
			if key == "" || strings.TrimSpace(key) != key || seen[key] {
				return fmt.Errorf("provider_access.providers must contain unique nonempty provider keys")
			}
			seen[key] = true
		}
	default:
		return fmt.Errorf("provider_access.mode must be all, selected or none")
	}
	return nil
}

// UnmarshalJSON distinguishes an omitted legacy policy from malformed input.
func (p *ProviderAccess) UnmarshalJSON(data []byte) error {
	type plain ProviderAccess
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.Mode == "" {
		return fmt.Errorf("provider_access.mode is required")
	}
	*p = ProviderAccess(value)
	return p.Validate()
}

// MarshalJSON makes the legacy default explicit in API responses.
func (p ProviderAccess) MarshalJSON() ([]byte, error) {
	type plain ProviderAccess
	if p.Mode == "" {
		p.Mode = "all"
	}
	return json.Marshal(plain(p))
}

// UnmarshalYAML rejects misspelled or incomplete persisted policies.
func (p *ProviderAccess) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("provider_access must be a policy object")
	}
	for i := 0; i < len(node.Content); i += 2 {
		if key := node.Content[i].Value; key != "mode" && key != "providers" {
			return fmt.Errorf("unknown provider_access field %q", key)
		}
	}
	type plain ProviderAccess
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	if value.Mode == "" {
		return fmt.Errorf("provider_access.mode is required")
	}
	*p = ProviderAccess(value)
	return p.Validate()
}
