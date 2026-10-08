package version

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// VersionDiscovery handles version discovery with a TTL-based cache
// (see internal/server/README.md for the Store / Tracker / Cache
// convention). TTLCache is correct here: remote version info changes
// externally with no in-process event, so TTL is the only defensible
// freshness signal.
type VersionDiscovery struct {
	client   *http.Client
	cache    map[string]*CachedVersionInfo
	cacheTTL time.Duration
	mu       sync.RWMutex
}

// CachedVersionInfo represents cached version information
type CachedVersionInfo struct {
	VersionInfo *VersionInfo
	CachedAt    time.Time
	TTL         time.Duration
}

// VersionCompatibilityError represents version compatibility errors
type VersionCompatibilityError struct {
	LocalVersion   *Version
	RemoteVersion  *Version
	NodeURL        string
	Errors         []string
	Warnings       []string
	ConnectionType string
}

func (e *VersionCompatibilityError) Error() string {
	return fmt.Sprintf("version compatibility error with %s: %s",
		e.NodeURL, e.Errors[0])
}

// GetUserFriendlyMessage returns a user-friendly error message with suggestions
func (e *VersionCompatibilityError) GetUserFriendlyMessage() string {
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("Version incompatibility with %s:\n", e.NodeURL))
	msg.WriteString(fmt.Sprintf("   Local: %s, Remote: %s\n",
		e.LocalVersion.String(), e.RemoteVersion.String()))

	for _, err := range e.Errors {
		msg.WriteString(fmt.Sprintf("   • %s\n", err))
	}

	if len(e.Warnings) > 0 {
		msg.WriteString("\nWarnings:\n")
		for _, warning := range e.Warnings {
			msg.WriteString(fmt.Sprintf("   %s\n", warning))
		}
	}

	return msg.String()
}

// GetUpgradeSuggestion provides specific upgrade suggestions
func (e *VersionCompatibilityError) GetUpgradeSuggestion() string {
	if e.LocalVersion.IsLessThan(e.RemoteVersion) {
		return fmt.Sprintf("Upgrade local service to version %s or later", e.RemoteVersion.String())
	}
	return fmt.Sprintf("Upgrade remote service (%s) to version %s or later",
		e.NodeURL, e.LocalVersion.String())
}

// NewVersionDiscovery creates a new version discovery service. No
// cluster authentication — version discovery hits the public /health
// endpoint which is unauthenticated.
func NewVersionDiscovery(cacheTTL time.Duration) *VersionDiscovery {
	return &VersionDiscovery{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		cache:    make(map[string]*CachedVersionInfo),
		cacheTTL: cacheTTL,
	}
}

// DiscoverVersion discovers version information from a remote service
func (vd *VersionDiscovery) DiscoverVersion(ctx context.Context, hostURL string) (*VersionInfo, error) {
	hostURL = NormalizeHostURL(hostURL)

	// Check cache first
	if cached := vd.getCachedVersion(hostURL); cached != nil {
		return cached, nil
	}

	// Discover version from remote service
	versionInfo, err := vd.discoverVersionFromRemote(ctx, hostURL)
	if err != nil {
		return nil, fmt.Errorf("failed to discover version from %s: %w", hostURL, err)
	}

	// Cache the result
	vd.cacheVersion(hostURL, versionInfo)

	return versionInfo, nil
}

// NormalizeHostURL prepends "http://" when the input is a bare host or
// host:port. net/url's parser rejects "192.0.2.10:9090/health" as
// "first path segment in URL cannot contain colon"; agents and CLIs
// that copy/paste an address out of /system or /cluster/validate
// pass exactly that form. IPv6 literals must be bracketed already
// (RFC 3986); we don't add brackets here, only the scheme.
func NormalizeHostURL(hostURL string) string {
	hostURL = strings.TrimSpace(hostURL)
	if hostURL == "" {
		return hostURL
	}
	if strings.HasPrefix(hostURL, "http://") || strings.HasPrefix(hostURL, "https://") {
		return hostURL
	}
	return "http://" + hostURL
}

// DiscoverVersionWithTimeout discovers version with a specific timeout
func (vd *VersionDiscovery) DiscoverVersionWithTimeout(hostURL string, timeout time.Duration) (*VersionInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return vd.DiscoverVersion(ctx, hostURL)
}

// ValidateCompatibility discovers version and validates compatibility
func (vd *VersionDiscovery) ValidateCompatibility(ctx context.Context, hostURL, connectionType string) (*CompatibilityResult, error) {
	// Discover remote version
	remoteVersion, err := vd.DiscoverVersion(ctx, hostURL)
	if err != nil {
		return nil, err
	}

	// Get local version info
	localVersion := GetServerVersionInfo()

	// Get compatibility matrix for connection type
	matrix := GetCompatibilityMatrix(connectionType)

	// Check compatibility
	result := CheckCompatibility(localVersion, remoteVersion, matrix)

	return result, nil
}

// ValidateCompatibilityWithError validates compatibility and returns detailed error if incompatible
func (vd *VersionDiscovery) ValidateCompatibilityWithError(ctx context.Context, hostURL, connectionType string) error {
	result, err := vd.ValidateCompatibility(ctx, hostURL, connectionType)
	if err != nil {
		return err
	}

	if !result.IsCompatible {
		remoteVersion, _ := vd.DiscoverVersion(ctx, hostURL)
		localVersion := GetServerVersionInfo()

		return &VersionCompatibilityError{
			LocalVersion:   localVersion.Version,
			RemoteVersion:  remoteVersion.Version,
			NodeURL:        hostURL,
			Errors:         result.Errors,
			Warnings:       result.Warnings,
			ConnectionType: connectionType,
		}
	}

	// Log warnings but don't fail
	if len(result.Warnings) > 0 {
		for _, warning := range result.Warnings {
			// TODO: Use proper logging
			fmt.Printf(" Version compatibility warning for %s: %s\n", hostURL, warning)
		}
	}

	return nil
}

// discoverVersionFromRemote performs the actual HTTP request to discover version.
//
// With a cluster key we prefer the authenticated internal endpoint, falling
// back to the unauthenticated /health probe so freshly joined workers (which
// may not yet have the cluster key installed) still resolve.
//
// Without a cluster key we can only hit /health — the public /zzrouter/v1/server/version
// routes all require auth, and /api/version is an Ollama-protocol stub that
// advertises a fake "0.0.0" when no Ollama daemon is configured.
func (vd *VersionDiscovery) discoverVersionFromRemote(ctx context.Context, hostURL string) (*VersionInfo, error) {
	var lastErr error
	for _, endpoint := range []string{"/health"} {
		url := hostURL + endpoint

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			lastErr = err
			continue
		}

		resp, err := vd.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
			continue
		}

		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			_ = resp.Body.Close()
			lastErr = err
			continue
		}
		_ = resp.Body.Close()

		// Try structured VersionInfo first (internal/version endpoint)
		var versionInfo VersionInfo
		rawBytes, _ := json.Marshal(raw)
		if err := json.Unmarshal(rawBytes, &versionInfo); err == nil && versionInfo.Version != nil {
			return &versionInfo, nil
		}

		// Fall back to /health response: parse "version" string field.
		// cluster_protocol / min_cluster_protocol land as JSON numbers
		// (float64 after encoding/json decoding into map[string]any);
		// extract them explicitly or the mesh compatibility gate will
		// see ClusterProtocol=0 on every peer and reject as
		// "pre-protocol build" — even when the peer advertises v1.
		if vs, ok := raw["version"].(string); ok && vs != "" {
			parsed, err := ParseVersion(vs)
			if err == nil {
				vi := &VersionInfo{
					Version:            parsed,
					APIVersion:         fmt.Sprint(raw["api_version"]),
					ServiceType:        fmt.Sprint(raw["service_type"]),
					ClusterProtocol:    jsonInt(raw["cluster_protocol"]),
					MinClusterProtocol: jsonInt(raw["min_cluster_protocol"]),
				}
				if caps, ok := raw["capabilities"].([]any); ok {
					for _, c := range caps {
						if s, ok := c.(string); ok {
							vi.Capabilities = append(vi.Capabilities, s)
						}
					}
				}
				return vi, nil
			}
		}

		lastErr = fmt.Errorf("no version information in response from %s", url)
		continue
	}

	return nil, fmt.Errorf("could not discover version from any endpoint: %w", lastErr)
}

// getCachedVersion retrieves cached version information if still valid
func (vd *VersionDiscovery) getCachedVersion(hostURL string) *VersionInfo {
	vd.mu.RLock()
	cached, exists := vd.cache[hostURL]
	if !exists {
		vd.mu.RUnlock()
		return nil
	}

	// Check if cache is still valid
	if time.Since(cached.CachedAt) > cached.TTL {
		vd.mu.RUnlock()
		// Cache expired, remove it with write lock
		vd.mu.Lock()
		// Double-check after acquiring write lock (another goroutine might have deleted it)
		if cached, exists := vd.cache[hostURL]; exists && time.Since(cached.CachedAt) > cached.TTL {
			delete(vd.cache, hostURL)
		}
		vd.mu.Unlock()
		return nil
	}

	result := cached.VersionInfo
	vd.mu.RUnlock()
	return result
}

// cacheVersion stores version information in cache
func (vd *VersionDiscovery) cacheVersion(hostURL string, versionInfo *VersionInfo) {
	vd.mu.Lock()
	defer vd.mu.Unlock()

	vd.cache[hostURL] = &CachedVersionInfo{
		VersionInfo: versionInfo,
		CachedAt:    utils.Now(),
		TTL:         vd.cacheTTL,
	}
}

// ClearCache clears the version cache
func (vd *VersionDiscovery) ClearCache() {
	vd.mu.Lock()
	defer vd.mu.Unlock()

	vd.cache = make(map[string]*CachedVersionInfo)
}

// jsonInt coerces a value from a decoded JSON map to int. JSON numbers
// arrive as float64 out of encoding/json; ints come through unchanged
// in test construction. Non-numeric → 0.
func jsonInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// InvalidateURL drops the cached version entry for a single host so
// the next DiscoverVersion call reprobes. No-op when no entry exists.
// Used by operator-triggered per-endpoint refreshes where clearing the
// whole cache would be over-broad.
func (vd *VersionDiscovery) InvalidateURL(hostURL string) {
	vd.mu.Lock()
	defer vd.mu.Unlock()
	delete(vd.cache, hostURL)
}

// GetCacheStats returns cache statistics
func (vd *VersionDiscovery) GetCacheStats() map[string]any {
	vd.mu.RLock()
	defer vd.mu.RUnlock()

	stats := map[string]any{
		"total_entries": len(vd.cache),
		"cache_ttl":     vd.cacheTTL.String(),
	}

	var validEntries, expiredEntries int
	now := utils.Now()

	for _, cached := range vd.cache {
		if now.Sub(cached.CachedAt) <= cached.TTL {
			validEntries++
		} else {
			expiredEntries++
		}
	}

	stats["valid_entries"] = validEntries
	stats["expired_entries"] = expiredEntries

	return stats
}
