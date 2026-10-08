// Package config provides OS-agnostic configuration file management for zzRouter
// App runtime configuration types and methods
package config

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
)

// AppRuntimeConfig represents runtime configuration in v2.0 format
type AppRuntimeConfig struct {
	Endpoint    string            `yaml:"endpoint"`
	PortRange   []int             `yaml:"port_range,omitempty"` // Port range for on-demand apps [start, end]
	BasePort    int               `yaml:"base_port,omitempty"`  // Starting port for dynamic allocation
	KeepAlive   string            `yaml:"keep_alive,omitempty"` // Duration to keep model loaded (e.g., "5m", "1h", "0" to disable, "-1" for indefinite)
	Execution   ExecutionConfig   `yaml:"execution"`
	HealthCheck HealthcheckConfig `yaml:"health_check,omitempty"`
	Resources   *ResourcesConfig  `yaml:"resources,omitempty"`
	Container   *ContainerConfig  `yaml:"container,omitempty"`
	API         *APIConfig        `yaml:"api,omitempty"` // For external API apps

	// Concurrency gating for on-demand instances.
	// 0 or omitted = unlimited (backward compatible). Only meaningful for on-demand providers;
	// external/service providers (e.g. Ollama) manage their own concurrency.
	MaxConcurrentRequests int `yaml:"max_concurrent_requests,omitempty"`
	// QueueTimeout bounds how long a request waits for a concurrency slot.
	// Absent = constants.QueueUntilCallerDeadline (wait as long as the caller
	// does), "0s" = reject immediately, "30s" = wait up to 30s.
	QueueTimeout string `yaml:"queue_timeout,omitempty"`

	// Models listing configuration (for cloud search)
	ModelsPath        string `yaml:"models_path,omitempty"`         // Path to list models (default: /v1/models)
	ModelsResponseKey string `yaml:"models_response_key,omitempty"` // JSON key containing model list (default: "data")
	ModelURLFmt       string `yaml:"model_url,omitempty"`           // URL template for model pages (e.g., "https://ollama.com/library/%s")
}

// ExecutionConfig represents execution configuration
type ExecutionConfig struct {
	Type       string   `yaml:"type"`                  // cli | python | api
	Command    string   `yaml:"command,omitempty"`     // Not used for type=api
	Args       []string `yaml:"args,omitempty"`        // Not used for type=api
	WorkingDir string   `yaml:"working_dir,omitempty"` // Not used for type=api

	// WireModel declares what the engine keys on in the "model" field of
	// wire payloads. Empty means WireModelName. See WireModel.
	WireModel WireModel `yaml:"wire_model,omitempty"`
}

// ContainerConfig represents container configuration
type ContainerConfig struct {
	Image    string   `yaml:"image"`
	Volumes  []string `yaml:"volumes"`
	Ports    []string `yaml:"ports"`
	Networks []string `yaml:"networks"`
	Restart  string   `yaml:"restart"`
}

// Values of auth_type: what a provider's endpoint is given to authenticate
// a request zzRouter forwards to it.
const (
	// AuthTypeNone: nothing. The endpoint asks for no credential, and the
	// caller's zzRouter key stops at zzRouter.
	AuthTypeNone = "none"
	// AuthTypeCaller: the caller's own key, unchanged, because the endpoint
	// checks it. What an external provider with no api block gets.
	AuthTypeCaller = "caller"
	// AuthTypeBearer, AuthTypeAPIKey and AuthTypeCustom: the provider's own
	// credential (token, custom_headers) in place of the caller's. An empty
	// auth_type is bearer.
	AuthTypeBearer = "bearer"
	AuthTypeAPIKey = "api-key"
	AuthTypeCustom = "custom"
)

// APIConfig represents API-specific configuration for external apps
type APIConfig struct {
	AuthType      string            `mapstructure:"auth_type" yaml:"auth_type"`     // see the AuthType constants
	AuthHeader    string            `mapstructure:"auth_header" yaml:"auth_header"` // Authorization, x-api-key, etc.
	AuthPrefix    string            `mapstructure:"auth_prefix" yaml:"auth_prefix"` // "Bearer ", empty, etc.
	Token         string            `mapstructure:"token" yaml:"token"`             // API key/token (supports ${ENV_VAR} syntax)
	CustomHeaders map[string]string `mapstructure:"custom_headers" yaml:"custom_headers,omitempty"`
	RateLimit     *RateLimitConfig  `mapstructure:"rate_limit" yaml:"rate_limit,omitempty"`
}

// ApplyAuthHeaders sets the provider's credential (token, custom_headers)
// on h, resolving ${ENV_VAR} references. backend.Upstream.Header is its
// caller; a request takes its credential from there.
func (ac *APIConfig) ApplyAuthHeaders(h http.Header) {
	if ac == nil {
		return
	}
	token := os.ExpandEnv(ac.Token)
	if token != "" {
		header := ac.AuthHeader
		if header == "" {
			header = "Authorization"
		}
		prefix := ac.AuthPrefix
		if prefix == "" && (ac.AuthType == AuthTypeBearer || ac.AuthType == "") {
			prefix = "Bearer "
		}
		h.Set(header, prefix+token)
	}
	for k, v := range ac.CustomHeaders {
		h.Set(k, os.ExpandEnv(v))
	}
}

// SendsOwnCredential reports whether the endpoint is given the provider's
// credential rather than nothing or the caller's key.
func (ac *APIConfig) SendsOwnCredential() bool {
	return ac != nil && ac.AuthType != AuthTypeNone && ac.AuthType != AuthTypeCaller
}

// Validate checks auth_type against the closed set, and that none and
// caller, which send no credential of the provider's, declare none.
func (ac *APIConfig) Validate() error {
	if ac == nil {
		return nil
	}
	switch ac.AuthType {
	case "", AuthTypeBearer, AuthTypeAPIKey, AuthTypeCustom:
		return nil
	case AuthTypeNone, AuthTypeCaller:
		if ac.Token != "" || ac.AuthHeader != "" || ac.AuthPrefix != "" || len(ac.CustomHeaders) > 0 {
			return fmt.Errorf("runtime.api.auth_type %q sends no credential of the provider's, so token, auth_header, auth_prefix and custom_headers must be empty", ac.AuthType)
		}
		return nil
	default:
		return fmt.Errorf("runtime.api.auth_type %q is not one of %s, %s, %s, %s, %s",
			ac.AuthType, AuthTypeBearer, AuthTypeAPIKey, AuthTypeCustom, AuthTypeNone, AuthTypeCaller)
	}
}

// HasCredentials returns true if the API config has resolvable credentials.
// Checks that token (after env expansion) is non-empty, or that at least one
// custom header with an env var reference resolves to a non-empty value.
// Static headers (like "anthropic-version: 2023-06-01") are not credentials.
func (ac *APIConfig) HasCredentials() bool {
	if ac == nil {
		return false
	}
	if ac.Token != "" && os.ExpandEnv(ac.Token) != "" {
		return true
	}
	for _, v := range ac.CustomHeaders {
		// Only headers that reference env vars (contain $) are credentials
		if strings.Contains(v, "$") && os.ExpandEnv(v) != "" {
			return true
		}
	}
	return false
}

// RateLimitConfig represents rate limiting settings
type RateLimitConfig struct {
	RequestsPerMinute int `mapstructure:"requests_per_minute" yaml:"requests_per_minute"`
	TokensPerMinute   int `mapstructure:"tokens_per_minute" yaml:"tokens_per_minute"`
}

// HealthcheckConfig represents healthcheck configuration
type HealthcheckConfig struct {
	Path     string   `yaml:"path,omitempty"`     // Health check path (e.g., /health, /v1/models)
	Test     []string `yaml:"test,omitempty"`     // Legacy Docker-style test command
	Interval string   `yaml:"interval,omitempty"` // Check interval (e.g., "30s")
	Timeout  string   `yaml:"timeout,omitempty"`  // Timeout per check (e.g., "5s")
	Retries  int      `yaml:"retries,omitempty"`  // Number of retries before marking unhealthy

	// Kubernetes-style health probes (for log-based health detection)
	ReadinessProbe *ProbeConfig `yaml:"readiness_probe,omitempty"` // Readiness probe configuration
	LivenessProbe  *ProbeConfig `yaml:"liveness_probe,omitempty"`  // Liveness probe configuration
}

// ProbeConfig represents a health probe configuration (readiness or liveness)
type ProbeConfig struct {
	LogPatterns         LogPatternsConfig `yaml:"logPatterns,omitempty"`         // Log patterns to match
	Timeout             string            `yaml:"timeout,omitempty"`             // Timeout for readiness probe
	InitialDelaySeconds int               `yaml:"initialDelaySeconds,omitempty"` // Initial delay for liveness probe

	// Serve declares a request that proves the engine can serve. Log
	// patterns and health endpoints report that the process is listening,
	// which for lazily-loading engines happens long before the weights are
	// usable. When set, readiness waits for this request to succeed.
	Serve *ServeCheckConfig `yaml:"serve,omitempty"`
}

// ServeCheckConfig is a request that only succeeds once the engine can
// actually serve inference. ${WIRE_MODEL} in Body expands to the token the
// engine keys on (see WireModel).
type ServeCheckConfig struct {
	Path   string `yaml:"path"`             // e.g. /v1/chat/completions
	Method string `yaml:"method,omitempty"` // default POST with a body, GET without
	Body   string `yaml:"body,omitempty"`   // JSON body; ${WIRE_MODEL} expanded
}

// LogPatternsConfig represents log patterns for health detection
type LogPatternsConfig struct {
	Success []PatternConfig `yaml:"success,omitempty"` // Success patterns
	Failure []PatternConfig `yaml:"failure,omitempty"` // Failure patterns
}

// PatternConfig represents a single pattern matcher
type PatternConfig struct {
	Pattern string `yaml:"pattern"` // Pattern to match
	IsRegex bool   `yaml:"isRegex"` // Whether pattern is a regex
}

// DeployConfig represents deployment configuration
type DeployConfig struct {
	Resources *ResourcesConfig `yaml:"resources,omitempty"`
}

// ResourcesConfig represents resource limits and reservations
type ResourcesConfig struct {
	Limits       AppResourceLimits    `yaml:"limits,omitempty"`
	Reservations ResourceReservations `yaml:"reservations,omitempty"`
}

// AppResourceLimits represents resource limits (renamed to avoid conflict)
type AppResourceLimits struct {
	Memory string `yaml:"memory,omitempty"`
	CPUs   string `yaml:"cpus,omitempty"`
}

// ResourceReservations represents resource reservations
type ResourceReservations struct {
	Devices []DeviceRequest `yaml:"devices,omitempty"`
}

// DeviceRequest represents a device request (e.g., GPU)
type DeviceRequest struct {
	Driver       string   `yaml:"driver,omitempty"`
	Count        int      `yaml:"count,omitempty"`
	Capabilities []string `yaml:"capabilities,omitempty"`
}

// VolumeConfig represents volume configuration
type VolumeConfig struct {
	Driver string `yaml:"driver,omitempty"`
}

// NetworkConfig represents network configuration
type NetworkConfig struct {
	Driver string `yaml:"driver,omitempty"`
}

// GetPortRange returns the port range for on-demand apps
// Returns [start, end] or nil if not configured
func (prc *AppRuntimeConfig) GetPortRange() []int {
	if len(prc.PortRange) == 2 {
		return prc.PortRange
	}
	return nil
}

// GetBasePort returns the base port for the app
// For on-demand apps, this is the starting port of the range
// For endpoint apps (service/external), this is the fixed port
func (prc *AppRuntimeConfig) GetBasePort() int {
	if prc.BasePort > 0 {
		return prc.BasePort
	}
	// Extract from PortRange if available
	if len(prc.PortRange) == 2 {
		return prc.PortRange[0]
	}
	return 0
}

// IsPortInRange checks if a port is within the app's port range
func (prc *AppRuntimeConfig) IsPortInRange(port int) bool {
	portRange := prc.GetPortRange()
	if portRange == nil {
		// No range defined, check against base port
		return port == prc.GetBasePort()
	}
	return port >= portRange[0] && port <= portRange[1]
}

// GetMaxConcurrentModels returns the maximum number of concurrent models
// based on the port range size
func (prc *AppRuntimeConfig) GetMaxConcurrentModels() int {
	portRange := prc.GetPortRange()
	if portRange == nil {
		return 1 // Always-on app, single instance
	}
	return portRange[1] - portRange[0] + 1
}

// GetKeepAliveDuration parses the keep_alive string and returns a time.Duration
// Returns:
//   - duration > 0: Keep model loaded for this duration after last use
//   - duration == 0: Unload immediately after use
//   - duration < 0: Keep loaded indefinitely
//   - error if parsing fails
func (prc *AppRuntimeConfig) GetKeepAliveDuration() (time.Duration, error) {
	if prc.KeepAlive == "" {
		return 5 * time.Minute, nil // Default: 5 minutes (like Ollama)
	}

	// Handle special values
	if prc.KeepAlive == "0" {
		return 0, nil // Unload immediately
	}
	if prc.KeepAlive == "-1" {
		return -1, nil // Keep indefinitely
	}

	// Parse duration string (e.g., "5m", "1h", "30s")
	duration, err := time.ParseDuration(prc.KeepAlive)
	if err != nil {
		return 0, fmt.Errorf("invalid keep_alive duration '%s': %w", prc.KeepAlive, err)
	}

	return duration, nil
}

// ShouldKeepAlive returns true if models should be kept loaded
func (prc *AppRuntimeConfig) ShouldKeepAlive() bool {
	duration, err := prc.GetKeepAliveDuration()
	if err != nil {
		return true // Default to keeping alive on error
	}
	return duration != 0 // Keep alive unless explicitly set to 0
}

// IsKeepAliveIndefinite returns true if models should be kept loaded indefinitely
func (prc *AppRuntimeConfig) IsKeepAliveIndefinite() bool {
	duration, err := prc.GetKeepAliveDuration()
	if err != nil {
		return false
	}
	return duration < 0
}

// GetQueueTimeout parses queue_timeout and returns a time.Duration.
// An absent value means constants.QueueUntilCallerDeadline; "0s" means
// reject immediately. An unparseable value (e.g. "30" with no unit) falls
// back to the default rather than to fail-fast — a typo shouldn't quietly
// turn a queue into a 429.
func (prc *AppRuntimeConfig) GetQueueTimeout() time.Duration {
	if prc.QueueTimeout == "" {
		return constants.QueueUntilCallerDeadline
	}
	d, err := time.ParseDuration(prc.QueueTimeout)
	if err != nil {
		slog.Warn("Invalid queue_timeout, using the caller's deadline", "value", prc.QueueTimeout, "error", err)
		return constants.QueueUntilCallerDeadline
	}
	return d
}
