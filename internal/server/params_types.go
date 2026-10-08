package server

import (
	"fmt"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// ============================================================================
// Parameter Types & Helpers
// ============================================================================
//
// These response types are legacy shims: the v5 surface is /resolved +
// /schema + PATCH /parameters. The three GET routes
// (/parameters, /nodes/parameters, /models/parameters) keep their old
// wire shape until TUI step 10 migrates callers to /resolved — this file
// synthesizes that shape out of Resolve() so there's no dual resolver.

// ParameterEntry represents a single parameter or environment variable
// with per-tier source metadata. Mirrored in
// internal/client/utils/params_client.go to keep the wire stable.
type ParameterEntry struct {
	Key              string   `json:"key"`
	Value            string   `json:"value"`
	Source           string   `json:"source,omitempty"`
	AutoResolvedHint string   `json:"auto_resolved_hint,omitempty"`
	Ignored          bool     `json:"ignored"`
	IsOverride       bool     `json:"is_override,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

type AppParametersResponse struct {
	Provider    string           `json:"provider"`
	Parameters  []ParameterEntry `json:"parameters"`
	Environment []ParameterEntry `json:"environment"`
}

type NodeParametersResponse struct {
	Provider    string           `json:"provider"`
	Node        string           `json:"node"`
	Parameters  []ParameterEntry `json:"parameters"`
	Environment []ParameterEntry `json:"environment"`
}

type ModelParametersResponse struct {
	Provider    string           `json:"provider"`
	Model       string           `json:"model"`
	Parameters  []ParameterEntry `json:"parameters"`
	Environment []ParameterEntry `json:"environment"`
}

// buildLegacyAppResponse builds the defaults-only shim view.
func buildLegacyAppResponse(provider string, r pkgConfig.ResolvedParams) *AppParametersResponse {
	return &AppParametersResponse{
		Provider:    provider,
		Parameters:  entriesFromResolved(r.Parameters),
		Environment: entriesFromResolved(r.Environment),
	}
}

// buildLegacyNodeResponse surfaces defaults + node tier.
func buildLegacyNodeResponse(provider, node string, r pkgConfig.ResolvedParams) *NodeParametersResponse {
	return &NodeParametersResponse{
		Provider:    provider,
		Node:        node,
		Parameters:  entriesFromResolved(r.Parameters),
		Environment: entriesFromResolved(r.Environment),
	}
}

// buildLegacyModelResponse surfaces defaults + node + model + node-model.
func buildLegacyModelResponse(provider, model string, r pkgConfig.ResolvedParams) *ModelParametersResponse {
	return &ModelParametersResponse{
		Provider:    provider,
		Model:       model,
		Parameters:  entriesFromResolved(r.Parameters),
		Environment: entriesFromResolved(r.Environment),
	}
}

// entriesFromResolved converts the resolver map to the legacy slice; a
// value an operator tier set is flagged as an override for wire-compat.
// The release's own tiers (defaults, model defaults) are not overrides.
func entriesFromResolved(rv map[string]pkgConfig.ResolvedValue) []ParameterEntry {
	var out []ParameterEntry
	for k, v := range rv {
		out = append(out, ParameterEntry{
			Key:        k,
			Value:      coerce(v.Value),
			Source:     v.Tier.String(),
			IsOverride: v.Tier > pkgConfig.TierModelDefault,
		})
	}
	return out
}

func coerce(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// getAppConfig is the small accessor used by every handler — keeps nil-
// checks centralised and the error text consistent.
func getAppConfig(appsConfig *pkgConfig.AppsConfig, app string) (*pkgConfig.ServiceConfig, error) {
	if appsConfig == nil {
		return nil, fmt.Errorf("providers config not loaded")
	}
	cfg, exists := appsConfig.LookupApp(app)
	if !exists {
		return nil, fmt.Errorf("provider '%s' not found", app)
	}
	return &cfg, nil
}
