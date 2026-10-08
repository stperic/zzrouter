package config

type ServeConfig struct {
	Name           string            `mapstructure:"name" yaml:"name"`
	Bind           string            `mapstructure:"bind" yaml:"bind"`
	Port           int               `mapstructure:"port" yaml:"port"`
	APIKey         string            `mapstructure:"api_key" yaml:"api_key,omitempty"`
	Master         bool              `mapstructure:"master" yaml:"master,omitempty"`                     // For backward compatibility
	Cluster        []ClusterEndpoint `mapstructure:"cluster" yaml:"cluster,omitempty"`                   // For backward compatibility
	MaxRequestSize int64             `mapstructure:"max_request_size" yaml:"max_request_size,omitempty"` // SECURITY: Max request body size in bytes (default: 10MB)

	// TLS configuration for HTTPS
	TLSCert string `mapstructure:"tls_cert" yaml:"tls_cert,omitempty"` // Path to TLS certificate file (PEM)
	TLSKey  string `mapstructure:"tls_key" yaml:"tls_key,omitempty"`   // Path to TLS private key file (PEM)
}

// IsTLSEnabled returns true if both TLS cert and key are configured.
func (sc *ServeConfig) IsTLSEnabled() bool {
	return sc.TLSCert != "" && sc.TLSKey != ""
}

// AdminConfig holds admin API configuration
type AdminConfig struct {
	// Deprecated: Use Key instead
	APIKey string `mapstructure:"api_key" yaml:"api_key,omitempty"`

	// Key can be either an environment variable name or literal value
	Key string `mapstructure:"key" yaml:"key,omitempty"`
}

// UserConfig holds user API configuration (read-only access)
type UserConfig struct {
	// Key can be either an environment variable name or literal value
	Key string `mapstructure:"key" yaml:"key,omitempty"`
}

// AuthConfig holds authentication configuration
type AuthConfig struct {
	// Admin API key (full access)
	AdminKey string `mapstructure:"admin_key" yaml:"admin_key"`

	// User API key (read-only access)
	UserKey string `mapstructure:"user_key" yaml:"user_key"`

	// ClusterNetworkKey is a pairing/bootstrap credential. It no longer
	// gates any request path — worker compat moved to the cluster mTLS
	// listener, where the transport (mTLS + OU=coordinator) is the gate.
	ClusterNetworkKey string `mapstructure:"cluster_network_key" yaml:"cluster_network_key,omitempty"`

	// RequireCompatAuth when true, rejects unauthenticated requests on
	// compatibility routes (/v1/*, /api/*). Default false for backward compat.
	RequireCompatAuth bool `mapstructure:"require_compat_auth" yaml:"require_compat_auth,omitempty"`

	// AllowAnonymousCloud lets unauthenticated compat requests reach
	// cloud-backed models. The flag is worded to allow rather than to
	// require so its zero value is the safe posture: a cloud model spends
	// the operator's upstream credits, and a request carrying no key
	// cannot be attributed to any budget or virtual key, so anonymous
	// access to one is opt-in. Local models stay anonymous either way.
	AllowAnonymousCloud bool `mapstructure:"allow_anonymous_cloud" yaml:"allow_anonymous_cloud,omitempty"`
}
