package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/stperic/common/mcp/oauth"
)

// ProviderBrowserConfig controls the opt-in, separate-origin native provider gateway.
type ProviderBrowserConfig struct {
	Origin string `mapstructure:"origin" yaml:"origin,omitempty"`
	Listen string `mapstructure:"listen" yaml:"listen,omitempty"`
}

// ListenAddress returns the loopback bind address; Origin controls enablement.
func (c ProviderBrowserConfig) ListenAddress() string {
	if c.Listen == "" {
		return "127.0.0.1:9092"
	}
	return c.Listen
}

// Validate keeps provider code off the administrative cookie hostname.
func (c ProviderBrowserConfig) Validate(auth OAuthConfig, node ServeConfig) error {
	const field = "coordinator.provider_browser"
	if c.Origin == "" {
		if c.Listen != "" {
			return fmt.Errorf("%s.listen requires origin", field)
		}
		return nil
	}
	if err := oauth.ValidateIssuer(c.Origin); err != nil {
		return fmt.Errorf("%s.origin: %w", field, err)
	}
	issuer, err := auth.EffectiveIssuer(node)
	if err != nil {
		return err
	}
	if issuer == "" {
		return fmt.Errorf("%s requires auth.oauth.enabled", field)
	}
	provider, _ := url.Parse(c.Origin)
	admin, _ := url.Parse(issuer)
	if strings.TrimSuffix(provider.Hostname(), ".") == strings.TrimSuffix(admin.Hostname(), ".") {
		return fmt.Errorf("%s.origin must use a different hostname from auth.oauth.issuer_url", field)
	}
	host, port, err := net.SplitHostPort(c.ListenAddress())
	if err != nil {
		return fmt.Errorf("%s.listen: %w", field, err)
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || ip.String() != host || err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return fmt.Errorf("%s.listen must be a canonical loopback IP and port", field)
	}
	return nil
}
