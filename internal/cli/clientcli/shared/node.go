package shared

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/stperic/zzrouter/pkg/apipath"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
)

// EnsureNodeOnline checks if the configured node is reachable via its health
// endpoint. Returns the node config on success or an error if unreachable.
func EnsureNodeOnline(clientConfig *pkgConfig.ClientConfig) (*pkgConfig.ClientNodeConfig, error) {
	host, err := clientConfig.GetNodeConfig()
	if err != nil {
		return nil, err
	}

	healthURL := host.Address + apipath.Health

	httpClient := &http.Client{Timeout: constants.HTTPShortTimeout}

	req, err := http.NewRequestWithContext(context.Background(), "GET", healthURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create health check request: %w", err)
	}

	if host.APIKey != "" {
		req.Header.Set("X-API-Key", host.APIKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("node '%s' is not reachable. Please start the server with 'zzrouter-node start'", host.Name)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("node '%s' returned status %d. Please check the server", host.Name, resp.StatusCode)
	}

	return host, nil
}

// ResolveUITheme returns the theme from env var or saved config.
func ResolveUITheme() ui.Theme {
	if envTheme := os.Getenv("ZZROUTER_THEME"); envTheme != "" {
		return ui.ResolveTheme(envTheme)
	}
	cm := pkgConfig.NewConfigManager("zzrouter")
	if cfg, err := cm.LoadClientConfig(); err == nil && cfg.Preferences.Theme != "" {
		return ui.ResolveTheme(cfg.Preferences.Theme)
	}
	return ui.ResolveTheme("auto")
}

// NodeAddress returns the configured node address for display (stripped of protocol prefix).
func NodeAddress() string {
	cm := pkgConfig.NewConfigManager("zzrouter")
	if cfg, err := cm.LoadClientConfig(); err == nil {
		addr := cfg.Node.Address
		return strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://")
	}
	return "unknown"
}
