package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
)

// RegisterCloudProvidersFromConfig auto-registers cloud search providers from provider config.
// Any cloud provider with an OpenAI-compatible protocol gets a generic /v1/models search.
// A search resolves its provider from appsConfig when it runs, so an edit
// to the endpoint or the credential applies without registering again.
func RegisterCloudProvidersFromConfig(appsConfig func() *config.AppsConfig) {
	cfg := appsConfig()
	if cfg == nil {
		return
	}
	// Kind-scoped iteration replaces the old `range cfg.Apps; if
	// svc.IsCloudProvider()` predicate. The typed map carries only
	// cloud providers — no filtering, no pointer-checks on Runtime.
	for key, cp := range cfg.CloudProviders() {
		// Skip providers already registered (e.g., custom implementations via init()).
		if IsCloudSearchProvider(Provider(key)) {
			continue
		}
		name := cp.Name
		if name == "" {
			name = key
		}
		RegisterCloudProvider(Provider(key), CloudProviderMeta{
			DisplayName:  name,
			Description:  fmt.Sprintf("%s - %s", name, cp.Description),
			BaseURL:      config.NormalizeEndpoint(cp.Runtime.Endpoint),
			ModelURLFmt:  cp.Runtime.ModelURL,
			SortOptions:  []string{"newest", "name"},
			ClientSorts:  []string{"name", "newest"},
			ClientFilter: true,
			HasTags:      false,
		}, newCloudModelSearchFunc(key, appsConfig))
	}
}

// newCloudModelSearchFunc returns a CloudSearchFunc that fetches models from a
// cloud provider's model listing endpoint. Results are decoded as generic maps
// to preserve all provider-specific fields (e.g., Anthropic's created_at,
// display_name, capabilities).
func newCloudModelSearchFunc(key string, appsConfig func() *config.AppsConfig) CloudSearchFunc {
	return func(ctx context.Context, query string, limit int) ([]any, error) {
		cfg := appsConfig()
		svc, _ := cfg.LookupApp(key)
		provider, ok := backend.Target(&svc)
		if !ok {
			return nil, fmt.Errorf("provider %q has no endpoint configured", key)
		}
		modelsPath, responseKey := "/v1/models", "data"
		if cp, ok := cfg.CloudProviders()[key]; ok {
			if cp.Runtime.ModelsPath != "" {
				modelsPath = cp.Runtime.ModelsPath
			}
			if cp.Runtime.ModelsResponseKey != "" {
				responseKey = cp.Runtime.ModelsResponseKey
			}
		}

		client := &http.Client{Timeout: 30 * time.Second}
		req, err := provider.NewRequest(ctx, http.MethodGet, modelsPath, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch models: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
		}

		// Decode response key into generic maps to preserve all provider fields
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}

		modelsJSON, ok := raw[responseKey]
		if !ok {
			return nil, fmt.Errorf("response missing %q key", responseKey)
		}

		var models []map[string]any
		if err := json.Unmarshal(modelsJSON, &models); err != nil {
			return nil, fmt.Errorf("failed to parse models: %w", err)
		}

		// Filter by query (match against both id and name)
		if query != "" {
			queryLower := strings.ToLower(query)
			var filtered []map[string]any
			for _, m := range models {
				id, _ := m["id"].(string)
				name, _ := m["name"].(string)
				if strings.Contains(strings.ToLower(id), queryLower) ||
					strings.Contains(strings.ToLower(name), queryLower) {
					filtered = append(filtered, m)
				}
			}
			models = filtered
		}

		if limit > 0 && len(models) > limit {
			models = models[:limit]
		}

		results := make([]any, len(models))
		for i, m := range models {
			results[i] = m
		}
		return results, nil
	}
}
