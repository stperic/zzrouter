package metadata

import "fmt"

// DownloadOwner is the closed set of download destination owners.
type DownloadOwner string

const (
	// OwnerCatalog owns files in the LLM model store.
	OwnerCatalog DownloadOwner = "catalog"
	// OwnerProviderStore owns files in a provider's declared model store.
	OwnerProviderStore DownloadOwner = "provider_store"
)

// DownloadTarget identifies a destination without carrying a filesystem path.
type DownloadTarget struct {
	Owner    DownloadOwner `json:"owner,omitempty"`
	Provider string        `json:"provider,omitempty"`
	Category string        `json:"category,omitempty"`
	Name     string        `json:"name,omitempty"`
}

// EffectiveOwner preserves catalog behavior for omitted targets.
func (t DownloadTarget) EffectiveOwner() DownloadOwner {
	if t.Owner == "" {
		return OwnerCatalog
	}
	return t.Owner
}

// Validate refuses unknown owners and selectors that do not belong to them.
func (t DownloadTarget) Validate() error {
	switch t.EffectiveOwner() {
	case OwnerCatalog:
		if t.Provider == "" && t.Category == "" && t.Name == "" {
			return nil
		}
	case OwnerProviderStore:
		if t.Provider != "" && t.Category != "" && t.Name != "" {
			return nil
		}
	}
	return fmt.Errorf("invalid download target owner or selectors")
}

// EffectiveRevision defaults an omitted HF revision to main.
func (r DownloadRequest) EffectiveRevision() string {
	if r.Revision == "" {
		return "main"
	}
	return r.Revision
}

// ValidateTarget validates the owner and its source-selection contract.
func (r DownloadRequest) ValidateTarget() error {
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if r.Target.EffectiveOwner() == OwnerProviderStore && (r.Weights == "" || r.Force || len(r.Features)+len(r.Want)+len(r.Optional) > 0) {
		return fmt.Errorf("provider download requires one exact file without catalog options")
	}
	return nil
}
