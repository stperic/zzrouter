package config

import (
	"fmt"
	"strconv"

	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/constants"
)

// OAuthConfig controls default-enabled coordinator browser login.
type OAuthConfig struct {
	// Enabled defaults to true when omitted.
	Enabled   *bool  `mapstructure:"enabled" yaml:"enabled,omitempty"`
	IssuerURL string `mapstructure:"issuer_url" yaml:"issuer_url,omitempty"`
}

// Validate requires an origin whose spelling survives browser serialization.
func (c OAuthConfig) Validate() error {
	if c.Enabled != nil && !*c.Enabled || c.IssuerURL == "" {
		return nil
	}
	if err := oauth.ValidateIssuer(c.IssuerURL); err != nil {
		return fmt.Errorf("auth.oauth.issuer_url: %w", err)
	}
	return nil
}

// EffectiveIssuer fixes browser login to the configured issuer or localhost.
// It never derives authority from request headers or changes persisted config.
func (c OAuthConfig) EffectiveIssuer(node ServeConfig) (string, error) {
	if c.Enabled != nil && !*c.Enabled {
		return "", nil
	}
	issuer := c.IssuerURL
	if issuer == "" {
		scheme := "http"
		if node.IsTLSEnabled() {
			scheme = "https"
		}
		port := node.Port
		if port == 0 {
			port = constants.DefaultZZROUTERPort
		}
		issuer = scheme + "://localhost"
		if !(scheme == "http" && port == constants.StandardHTTPPort || scheme == "https" && port == constants.StandardHTTPSPort) {
			issuer += ":" + strconv.Itoa(port)
		}
	}
	if err := (OAuthConfig{IssuerURL: issuer}).Validate(); err != nil {
		return "", err
	}
	return issuer, nil
}
