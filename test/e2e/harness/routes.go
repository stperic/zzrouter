package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// RouteSpec mirrors the wire shape returned by the server's
// GET /zzrouter/v1/internal/server/routes endpoint. The fields are kept
// flat + JSON-tagged so the harness can decode without depending on
// internal/server types (which would force a back-import the harness
// must not have).
type RouteSpec struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Surface   string `json:"surface"`
	Auth      string `json:"auth"`
	Streaming string `json:"streaming,omitempty"`
	Async     bool   `json:"async,omitempty"`
	// Retired: mounted, but only ever answers 410 Gone. A suite that
	// treats one as an ordinary route writes a success-path test for an
	// outcome the server will never produce.
	Retired bool `json:"retired,omitempty"`
}

// RouteCatalog wraps the server response. Count is asserted against
// len(Routes) at decode time — a desync there is the cleanest signal
// that the wire shape changed.
type RouteCatalog struct {
	Routes []RouteSpec `json:"routes"`
	Count  int         `json:"count"`
}

// FetchRoutes hits GET <baseURL>/zzrouter/v1/server/routes with the
// admin API key and decodes the catalog. baseURL must NOT include a
// trailing slash. The endpoint lives on the public-admin surface
// (the public-engine /internal/* mount is gated for in-process
// dispatch only; mTLS-gated /internal/* on the cluster listener
// isn't reachable from a plain HTTP client).
//
// httpClient is injected so callers can wrap with a cassette recorder,
// retry middleware, or a test transport. Pass nil for the default
// client (no timeout set; caller controls via ctx).
func FetchRoutes(ctx context.Context, httpClient *http.Client, baseURL, adminKey string) (*RouteCatalog, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("baseURL is required")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	u, err := url.Parse(baseURL + "/zzrouter/v1/server/routes")
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
		return nil, fmt.Errorf("get routes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get routes: status %d", resp.StatusCode)
	}

	var cat RouteCatalog
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		return nil, fmt.Errorf("decode routes: %w", err)
	}
	if cat.Count != len(cat.Routes) {
		return nil, fmt.Errorf("route catalog count mismatch: header=%d, body=%d", cat.Count, len(cat.Routes))
	}
	// Server returns sorted; we trust it. Re-sorting here would mask a
	// server-side regression in stable ordering.
	return &cat, nil
}

// PartitionRoutes splits the catalog by mutation impact for Ring 1's
// parallelism rule: read-only routes parallelize freely; mutating
// routes serialize per surface. The partition is conservative — any
// POST/PATCH/PUT/DELETE counts as mutating, plus a small allowlist of
// non-idempotent GETs that touch shared state (none today).
func PartitionRoutes(routes []RouteSpec) (readOnly, mutating []RouteSpec) {
	for _, r := range routes {
		if IsMutating(r) {
			mutating = append(mutating, r)
		} else {
			readOnly = append(readOnly, r)
		}
	}
	return
}

// IsMutating reports whether a route can mutate server state. Method-only
// today; extend with a known-mutating-GET allowlist when one is found.
func IsMutating(r RouteSpec) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// GroupBySurface returns routes grouped by their Surface tag. Stable
// key iteration is the caller's responsibility (range a sorted slice
// of keys).
func GroupBySurface(routes []RouteSpec) map[string][]RouteSpec {
	out := map[string][]RouteSpec{}
	for _, r := range routes {
		out[r.Surface] = append(out[r.Surface], r)
	}
	return out
}
