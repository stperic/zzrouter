package config

import "fmt"

// SecurityConfig holds security-related configuration
type SecurityConfig struct {
	// LogAuthAttempts logs authentication attempts for debugging
	LogAuthAttempts bool `mapstructure:"log_auth_attempts" yaml:"log_auth_attempts,omitempty"`

	// CORS governs cross-origin browser access to the HTTP API. Off by
	// default; operators opt in per deployment.
	CORS CORSConfig `mapstructure:"cors" yaml:"cors,omitempty"`
}

// CORSConfig configures the CORS middleware. When Enabled is false the
// middleware is a zero-cost passthrough and no Access-Control-* headers
// are ever emitted.
type CORSConfig struct {
	Enabled          bool     `mapstructure:"enabled" yaml:"enabled"`
	AllowedOrigins   []string `mapstructure:"allowed_origins" yaml:"allowed_origins,omitempty"`
	AllowedMethods   []string `mapstructure:"allowed_methods" yaml:"allowed_methods,omitempty"`
	AllowedHeaders   []string `mapstructure:"allowed_headers" yaml:"allowed_headers,omitempty"`
	ExposedHeaders   []string `mapstructure:"exposed_headers" yaml:"exposed_headers,omitempty"`
	AllowCredentials bool     `mapstructure:"allow_credentials" yaml:"allow_credentials,omitempty"`
	MaxAgeSeconds    int      `mapstructure:"max_age_seconds" yaml:"max_age_seconds,omitempty"`
}

// Validate enforces the invariants the middleware relies on. Disabled
// configs skip validation so operators can stage defaults without
// tripping checks.
func (c CORSConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if len(c.AllowedOrigins) == 0 {
		return fmt.Errorf("security.cors.allowed_origins must be set when CORS is enabled")
	}
	hasWildcard := false
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			hasWildcard = true
			break
		}
	}
	if hasWildcard && c.AllowCredentials {
		return fmt.Errorf("security.cors.allow_credentials cannot be true when allowed_origins includes \"*\"")
	}
	if c.MaxAgeSeconds < 0 {
		return fmt.Errorf("security.cors.max_age_seconds must be >= 0, got %d", c.MaxAgeSeconds)
	}
	return nil
}
