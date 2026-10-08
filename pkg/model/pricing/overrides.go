package pricing

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"gopkg.in/yaml.v3"
)

// ErrOverrideNoModel is returned when an override arrives without a model.
var ErrOverrideNoModel = errors.New("override requires a model")

// ErrOverrideNegative is returned for a negative rate. Zero is legal and
// meaningful (see Override.Note on the zero case); negative is not.
var ErrOverrideNegative = errors.New("override rates must not be negative")

// WildcardModel matches every model under a provider. Only meaningful
// with Provider set: "ollama-cloud/*" prices a subscription-billed
// provider that publishes no per-token rate card, which is otherwise
// un-priceable one model at a time.
const WildcardModel = "*"

// Override is an operator-supplied rate for a model the upstream table
// prices wrongly, or does not price at all. It wins over the upstream
// table unconditionally, including when every rate is zero: an all-zero
// override is how an operator declares "this provider does not bill per
// token, stop reporting it as un-priced".
type Override struct {
	// Provider scopes the override. Empty matches any provider, which is
	// the right shape for a model id that means the same thing wherever
	// it is served.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// Model is the model id, or WildcardModel for every model under Provider.
	Model string `yaml:"model" json:"model"`

	InputCostPerToken     float64 `yaml:"input_cost_per_token" json:"input_cost_per_token"`
	OutputCostPerToken    float64 `yaml:"output_cost_per_token" json:"output_cost_per_token"`
	CacheReadCostPerToken float64 `yaml:"cache_read_input_token_cost,omitempty" json:"cache_read_input_token_cost,omitempty"`

	Note      string    `yaml:"note,omitempty" json:"note,omitempty"`
	UpdatedAt time.Time `yaml:"updated_at,omitempty" json:"updated_at,omitempty"`
	UpdatedBy string    `yaml:"updated_by,omitempty" json:"updated_by,omitempty"`
}

// Key is the map/storage key: "provider/model", or bare "model" when the
// override is provider-agnostic. Lowercased to match the store's
// case-insensitive lookups.
func (o Override) Key() string {
	m := strings.ToLower(strings.TrimSpace(o.Model))
	p := strings.ToLower(strings.TrimSpace(o.Provider))
	if p == "" {
		return m
	}
	return p + "/" + m
}

// Pricing renders the override as a ModelPricing so it can be returned
// from the same lookup paths as upstream data.
func (o Override) Pricing() ModelPricing {
	return ModelPricing{
		InputCostPerToken:     o.InputCostPerToken,
		OutputCostPerToken:    o.OutputCostPerToken,
		CacheReadCostPerToken: o.CacheReadCostPerToken,
		Provider:              o.Provider,
		Mode:                  "chat",
	}
}

func (o Override) validate() error {
	if strings.TrimSpace(o.Model) == "" {
		return ErrOverrideNoModel
	}
	if o.Model == WildcardModel && strings.TrimSpace(o.Provider) == "" {
		return fmt.Errorf("%w: wildcard model requires a provider", ErrOverrideNoModel)
	}
	if o.InputCostPerToken < 0 || o.OutputCostPerToken < 0 || o.CacheReadCostPerToken < 0 {
		return ErrOverrideNegative
	}
	return nil
}

// overridesFile is the on-disk shape of pricing_overrides.yaml.
type overridesFile struct {
	Version   string     `yaml:"version"`
	Overrides []Override `yaml:"overrides"`
}

const overridesFileVersion = "1"

// LoadOverrides records the override file path and loads it. A missing
// file is not an error — the path is retained so the first write creates
// it, matching how the model-group store treats its config.
func (s *Store) LoadOverrides(path string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.overridesPath = path
	s.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read pricing overrides: %w", err)
	}

	var f overridesFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse pricing overrides: %w", err)
	}

	loaded := make(map[string]Override, len(f.Overrides))
	for _, o := range f.Overrides {
		if err := o.validate(); err != nil {
			return fmt.Errorf("pricing override %q: %w", o.Key(), err)
		}
		loaded[o.Key()] = o
	}

	s.mu.Lock()
	s.overrides = loaded
	s.mu.Unlock()
	return nil
}

// OverridesPath returns the file the overrides persist to, empty when
// the store is memory-only.
func (s *Store) OverridesPath() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.overridesPath
}

// SetOverride adds or replaces an override and persists the set. The
// stored copy is stamped with UpdatedAt when the caller left it zero.
func (s *Store) SetOverride(o Override) error {
	if s == nil {
		return errors.New("pricing store is not enabled")
	}
	if err := o.validate(); err != nil {
		return err
	}
	o.Model = strings.TrimSpace(o.Model)
	o.Provider = strings.TrimSpace(o.Provider)
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = utils.Now()
	}

	s.mu.Lock()
	if s.overrides == nil {
		s.overrides = make(map[string]Override)
	}
	s.overrides[o.Key()] = o
	s.mu.Unlock()

	// An override that prices a model retroactively answers the miss that
	// put it on the operator's list; drop the tally so the list reflects
	// what is still unresolved.
	s.clearMiss(o.Key())

	return s.saveOverrides()
}

// DeleteOverride removes an override. Reports whether one was present.
func (s *Store) DeleteOverride(provider, model string) (bool, error) {
	if s == nil {
		return false, errors.New("pricing store is not enabled")
	}
	key := Override{Provider: provider, Model: model}.Key()

	s.mu.Lock()
	_, existed := s.overrides[key]
	delete(s.overrides, key)
	s.mu.Unlock()

	if !existed {
		return false, nil
	}
	return true, s.saveOverrides()
}

// ListOverrides returns every override, ordered by key for stable output.
func (s *Store) ListOverrides() []Override {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	out := make([]Override, 0, len(s.overrides))
	for _, o := range s.overrides {
		out = append(out, o)
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// LookupOverride returns the operator override covering (provider, model),
// most specific first: exact provider+model, then the provider wildcard,
// then a provider-agnostic entry for the bare model.
//
// Distinct from Lookup because an all-zero override is a deliberate "not
// billed per token" declaration, and callers that gate on ModelPricing
// having a non-zero rate would otherwise discard it as a miss.
func (s *Store) LookupOverride(provider, model string) (ModelPricing, bool) {
	if s == nil {
		return ModelPricing{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.lookupOverrideLocked(provider, model)
	if !ok {
		return ModelPricing{}, false
	}
	return o.Pricing(), true
}

// lookupOverrideLocked is the shared probe. Callers hold s.mu.
func (s *Store) lookupOverrideLocked(provider, model string) (Override, bool) {
	if len(s.overrides) == 0 {
		return Override{}, false
	}
	prov := strings.ToLower(strings.TrimSpace(provider))
	mod := strings.ToLower(strings.TrimSpace(model))
	if mod == "" {
		return Override{}, false
	}

	if prov != "" {
		if o, ok := s.overrides[prov+"/"+mod]; ok {
			return o, true
		}
		if o, ok := s.overrides[prov+"/"+WildcardModel]; ok {
			return o, true
		}
	}
	o, ok := s.overrides[mod]
	return o, ok
}

// saveOverrides writes the current set to the configured path. A store
// with no path (tests, in-memory use) keeps the change in memory.
func (s *Store) saveOverrides() error {
	s.mu.RLock()
	path := s.overridesPath
	f := overridesFile{Version: overridesFileVersion, Overrides: make([]Override, 0, len(s.overrides))}
	for _, o := range s.overrides {
		f.Overrides = append(f.Overrides, o)
	}
	s.mu.RUnlock()

	if path == "" {
		return nil
	}
	sort.Slice(f.Overrides, func(i, j int) bool { return f.Overrides[i].Key() < f.Overrides[j].Key() })

	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode pricing overrides: %w", err)
	}
	if err := utils.AtomicWriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write pricing overrides: %w", err)
	}
	return nil
}
