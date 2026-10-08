package client

import (
	"net/url"
	"time"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// PricingOverride is an operator-supplied rate for a model the upstream
// table prices wrongly or not at all. Rates are dollars per million
// tokens, the unit the API speaks.
type PricingOverride struct {
	Provider           string    `json:"provider,omitempty"`
	Model              string    `json:"model"`
	InputCostPer1M     float64   `json:"input_cost_per_1m"`
	OutputCostPer1M    float64   `json:"output_cost_per_1m"`
	CacheReadCostPer1M float64   `json:"cache_read_cost_per_1m,omitempty"`
	Note               string    `json:"note,omitempty"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
	UpdatedBy          string    `json:"updated_by,omitempty"`

	// BilledPerToken is false when every rate is zero, which is how an
	// operator marks a provider that bills by subscription or not at all.
	BilledPerToken bool `json:"billed_per_token"`
	// AppliesToAllModels is true for a provider-wide wildcard entry.
	AppliesToAllModels bool `json:"applies_to_all_models"`
}

// PricingOverrideListResponse is the envelope for GET /pricing/overrides.
type PricingOverrideListResponse struct {
	Data struct {
		Overrides []PricingOverride `json:"overrides"`
		Count     int               `json:"count"`
	} `json:"data"`
}

// PricingOverrideDetailResponse is the envelope for a single override.
type PricingOverrideDetailResponse struct {
	Data PricingOverride `json:"data"`
}

// PricingStatus is the envelope for GET /pricing/status. Unpriced maps
// "provider/model" to the number of requests that could not be priced —
// each one settled at $0 and drew down no budget.
type PricingStatus struct {
	Enabled       bool             `json:"enabled"`
	ModelCount    int              `json:"model_count"`
	FetchedAt     time.Time        `json:"fetched_at,omitempty"`
	UnpricedCount int              `json:"unpriced_count"`
	Unpriced      map[string]int64 `json:"unpriced,omitempty"`
}

type pricingStatusResponse struct {
	Data PricingStatus `json:"data"`
}

// GetPricingStatus reports catalog freshness and what went un-priced.
// GET /zzrouter/v1/pricing/status
func (c *Client) GetPricingStatus() (*PricingStatus, error) {
	var result pricingStatusResponse
	if err := c.doJSON("GET", apipath.PricingStatus, nil, &result, "get pricing status"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// ListPricingOverrides returns every operator price override.
// GET /zzrouter/v1/pricing/overrides
func (c *Client) ListPricingOverrides() ([]PricingOverride, error) {
	var result PricingOverrideListResponse
	if err := c.doJSON("GET", apipath.PricingOverrides, nil, &result, "list pricing overrides"); err != nil {
		return nil, err
	}
	return result.Data.Overrides, nil
}

// SetPricingOverride adds or replaces an override.
// POST /zzrouter/v1/pricing/overrides
func (c *Client) SetPricingOverride(o *PricingOverride) (*PricingOverride, error) {
	var result PricingOverrideDetailResponse
	if err := c.doJSON("POST", apipath.PricingOverrides, o, &result, "save pricing override"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// DeletePricingOverride removes an override, restoring upstream pricing.
// DELETE /zzrouter/v1/pricing/overrides?model=&provider=
//
// Both values are query-escaped: model ids carry slashes and "@" that
// would otherwise reshape the request path.
func (c *Client) DeletePricingOverride(provider, model string) error {
	q := url.Values{}
	q.Set("model", model)
	if provider != "" {
		q.Set("provider", provider)
	}
	path := apipath.PricingOverrides + "?" + q.Encode()
	return c.doJSON("DELETE", path, nil, nil, "delete pricing override")
}
