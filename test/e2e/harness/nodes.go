package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// NodeInfo mirrors a single entry in the wire response of
// GET /zzrouter/v1/nodes. The server returns []map[string]any to keep
// the public surface flexible; the harness decodes only the fields it
// asserts on. Raw carries the full map for tests that need anything
// outside this typed view.
type NodeInfo struct {
	Name         string         `json:"name"`
	Role         string         `json:"role,omitempty"`
	HealthStatus string         `json:"health_status,omitempty"`
	IsLocal      bool           `json:"is_local,omitempty"`
	Address      string         `json:"address,omitempty"`
	Raw          map[string]any `json:"-"`
}

// nodesListResponse mirrors response_helpers.ListResponse for /nodes
// (server returns Data: []map[string]any). We unmarshal into the maps
// then project the typed view in FetchNodes — this keeps the harness
// from depending on internal/server's NodeInfo shape.
type nodesListResponse struct {
	Data    []map[string]any `json:"data"`
	Total   int              `json:"total"`
	HasMore bool             `json:"has_more,omitempty"`
}

// FetchNodes hits GET <baseURL>/zzrouter/v1/nodes with the admin key
// and returns the cluster's known nodes — local coord plus any paired
// workers from the EndpointRegistry. Used by Ring 2's suite() to fail
// fast when the operator hasn't completed manual pairing.
//
// Pass nil httpClient for the default; callers control timeout via ctx.
// baseURL must NOT include a trailing slash.
func FetchNodes(ctx context.Context, httpClient *http.Client, baseURL, adminKey string) ([]NodeInfo, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("baseURL is required")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	u, err := url.Parse(baseURL + "/zzrouter/v1/nodes")
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if adminKey != "" {
		req.Header.Set("X-API-Key", adminKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get nodes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get nodes: status %d", resp.StatusCode)
	}

	var body nodesListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode nodes: %w", err)
	}

	out := make([]NodeInfo, 0, len(body.Data))
	for _, m := range body.Data {
		ni := NodeInfo{Raw: m}
		if v, ok := m["name"].(string); ok {
			ni.Name = v
		}
		if v, ok := m["role"].(string); ok {
			ni.Role = v
		}
		if v, ok := m["health_status"].(string); ok {
			ni.HealthStatus = v
		}
		if v, ok := m["is_local"].(bool); ok {
			ni.IsLocal = v
		}
		if v, ok := m["address"].(string); ok {
			ni.Address = v
		}
		out = append(out, ni)
	}
	return out, nil
}

// RequireNodes asserts that every name in want is present in have.
// Returns a single error listing missing names, or nil. Names are
// compared case-insensitively to match the server's ListNodes filter
// behavior.
func RequireNodes(have []NodeInfo, want []string) error {
	if len(want) == 0 {
		return nil
	}
	present := make(map[string]bool, len(have))
	for _, n := range have {
		present[lowerASCII(n.Name)] = true
	}
	var missing []string
	for _, w := range want {
		if !present[lowerASCII(w)] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("nodes missing from /zzrouter/v1/nodes: %v (have: %v)", missing, names(have))
	}
	return nil
}

func names(ns []NodeInfo) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Name)
	}
	return out
}

// lowerASCII matches the server's strings.EqualFold-style filter used
// by ListNodes; we don't need full Unicode case folding here.
func lowerASCII(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
