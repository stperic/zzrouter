package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/security"
)

// UpdateConfig holds auto-update configuration for the node
type UpdateConfig struct {
	// Enabled controls whether automatic updates are enabled
	// Default: true
	Enabled *bool `mapstructure:"enabled" yaml:"enabled,omitempty"`

	// Channel is the release channel to follow: "stable", "beta"
	// Default: "stable"
	Channel string `mapstructure:"channel" yaml:"channel,omitempty"`

	// CheckIntervalHours is how often to check for updates (in hours)
	// Default: 24
	CheckIntervalHours int `mapstructure:"check_interval_hours" yaml:"check_interval_hours,omitempty"`

	// MaintenanceWindow is a cron expression defining when updates can be applied
	// If empty, updates are applied immediately when available
	// Example: "0 3 * * *" = apply updates at 3 AM only
	MaintenanceWindow string `mapstructure:"maintenance_window" yaml:"maintenance_window,omitempty"`

	// PinnedVersion pins the node to a specific version pattern
	// Supports patterns like "1.2.x", "1.x", or exact "1.2.3"
	// Empty means no pinning (always update to latest in channel)
	PinnedVersion string `mapstructure:"pinned_version" yaml:"pinned_version,omitempty"`

	// AutoRestart controls whether to automatically restart after update
	// Default: true
	AutoRestart *bool `mapstructure:"auto_restart" yaml:"auto_restart,omitempty"`

	// KeepPreviousVersions is the number of previous versions to keep for rollback
	// Default: 2
	KeepPreviousVersions int `mapstructure:"keep_previous_versions" yaml:"keep_previous_versions,omitempty"`

	// Source overrides where releases are fetched from. Absent means the
	// public zzRouter releases, which is what a real deployment wants.
	// It exists so an update can be rehearsed against a local or staging
	// release feed instead of a published one.
	Source *UpdateSourceConfig `mapstructure:"source" yaml:"source,omitempty"`
}

// UpdateSourceConfig points the updater at a release feed other than the
// public one.
//
// This decides which binary the node will install and run, so it is
// deliberately config-only: no environment variable and no API can set
// it, and a node running with it announces the fact in its update
// status and in its logs.
type UpdateSourceConfig struct {
	// APIBaseURL replaces https://api.github.com. The updater appends
	// /repos/<owner>/<repo>/releases to it.
	APIBaseURL string `mapstructure:"api_base_url" yaml:"api_base_url,omitempty"`

	// Owner and Repo replace the stperic/zzrouter coordinates.
	Owner string `mapstructure:"owner" yaml:"owner,omitempty"`
	Repo  string `mapstructure:"repo" yaml:"repo,omitempty"`

	// AllowedHosts replaces the github.com download allowlist. Entries
	// are host or host:port; the HTTPS-only rule still applies, so a
	// local feed needs a certificate the node trusts.
	AllowedHosts []string `mapstructure:"allowed_hosts" yaml:"allowed_hosts,omitempty"`
}

// Describe renders the source for logs and update status.
func (c *UpdateSourceConfig) Describe() string {
	return fmt.Sprintf("%s/%s at %s", c.Owner, c.Repo, c.APIBaseURL)
}

// IsLoopbackOnly reports whether every allowed download host is an
// address that cannot leave this machine. Such a feed may serve over
// plaintext: the hop never reaches a network, and requiring a
// certificate the node trusts would mean adding one to the operator's
// system trust store just to rehearse an update. One non-loopback entry
// takes the whole source back to HTTPS-only.
func (c *UpdateSourceConfig) IsLoopbackOnly() bool {
	if len(c.AllowedHosts) == 0 {
		return false
	}
	for _, host := range c.AllowedHosts {
		hostname := host
		if h, _, err := net.SplitHostPort(host); err == nil {
			hostname = h
		}
		if !security.IsLoopbackHost(hostname) {
			return false
		}
	}
	return true
}

// InferenceLogConfig controls the in-memory inference request log.
type InferenceLogConfig struct {
	// Enabled controls whether inference logging is active.
	// Default: true (logs are captured when section is absent)
	Enabled *bool `mapstructure:"enabled" yaml:"enabled,omitempty"`

	// MaxEntries is the ring buffer capacity. Default: 1000
	MaxEntries int `mapstructure:"max_entries" yaml:"max_entries,omitempty"`

	// CapturePrompts controls whether prompt content is stored.
	// Default: true (self-hosted system, admin-key gated)
	CapturePrompts *bool `mapstructure:"capture_prompts" yaml:"capture_prompts,omitempty"`

	// MaxPayloads is how many of the most recent entries retain their
	// full request bodies. Default: 100. Set to 0 to disable retention.
	MaxPayloads *int `mapstructure:"max_payloads" yaml:"max_payloads,omitempty"`
}

// IsEnabled returns true if inference logging is enabled (default: true).
func (c *InferenceLogConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// GetMaxEntries returns the configured max entries or the ring default.
func (c *InferenceLogConfig) GetMaxEntries() int {
	if c.MaxEntries <= 0 {
		return inferencelog.DefaultMaxEntries
	}
	return c.MaxEntries
}

// ShouldCapturePrompts returns true if prompt capture is enabled (default: true).
func (c *InferenceLogConfig) ShouldCapturePrompts() bool {
	if c.CapturePrompts == nil {
		return true
	}
	return *c.CapturePrompts
}

// GetMaxPayloads returns the payload retention window, honoring the
// ZZROUTER_INFERENCE_LOG_PAYLOADS override. Returns 0 when prompt
// capture is off — payloads carry the same prompt content.
func (c *InferenceLogConfig) GetMaxPayloads() int {
	if !c.ShouldCapturePrompts() {
		return 0
	}
	if env := os.Getenv("ZZROUTER_INFERENCE_LOG_PAYLOADS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n >= 0 {
			return n
		}
		slog.Warn("Ignoring invalid ZZROUTER_INFERENCE_LOG_PAYLOADS", "value", env)
	}
	if c.MaxPayloads == nil {
		return inferencelog.DefaultMaxPayloads
	}
	if *c.MaxPayloads < 0 {
		return 0
	}
	return *c.MaxPayloads
}

// IsEnabled returns true if auto-update is enabled (default: true)
func (c *UpdateConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true // Default: enabled
	}
	return *c.Enabled
}

// GetChannel returns the update channel (default: "stable")
func (c *UpdateConfig) GetChannel() string {
	if c.Channel == "" {
		return "stable"
	}
	return c.Channel
}

// GetCheckIntervalHours returns the check interval in hours (default: 24)
func (c *UpdateConfig) GetCheckIntervalHours() int {
	if c.CheckIntervalHours <= 0 {
		return 24
	}
	return c.CheckIntervalHours
}

// IsAutoRestartEnabled returns true if auto-restart is enabled (default: true)
func (c *UpdateConfig) IsAutoRestartEnabled() bool {
	if c.AutoRestart == nil {
		return true // Default: enabled
	}
	return *c.AutoRestart
}

// GetKeepPreviousVersions returns the number of versions to keep (default: 2)
func (c *UpdateConfig) GetKeepPreviousVersions() int {
	if c.KeepPreviousVersions <= 0 {
		return 2
	}
	return c.KeepPreviousVersions
}

// HasMaintenanceWindow returns true if a maintenance window is configured
func (c *UpdateConfig) HasMaintenanceWindow() bool {
	return c.MaintenanceWindow != ""
}

// HasPinnedVersion returns true if a version pin is configured
func (c *UpdateConfig) HasPinnedVersion() bool {
	return c.PinnedVersion != ""
}

// Validate validates the UpdateConfig and returns an error if invalid
func (c *UpdateConfig) Validate() error {
	// Validate channel
	if c.Channel != "" && c.Channel != "stable" && c.Channel != "beta" {
		return fmt.Errorf("%w: invalid update channel: %q (must be 'stable' or 'beta')", ErrInvalidConfig, c.Channel)
	}

	// Validate check interval
	if c.CheckIntervalHours < 0 {
		return fmt.Errorf("%w: check_interval_hours cannot be negative: %d", ErrInvalidConfig, c.CheckIntervalHours)
	}

	// Validate maintenance window if present
	if c.MaintenanceWindow != "" {
		if err := ValidateMaintenanceWindow(c.MaintenanceWindow); err != nil {
			return fmt.Errorf("%w: invalid maintenance_window: %w", ErrInvalidConfig, err)
		}
	}

	// Validate pinned version if present
	if c.PinnedVersion != "" {
		if err := ValidateVersionPin(c.PinnedVersion); err != nil {
			return fmt.Errorf("%w: invalid pinned_version: %w", ErrInvalidConfig, err)
		}
	}

	// Validate keep previous versions
	if c.KeepPreviousVersions < 0 {
		return fmt.Errorf("%w: keep_previous_versions cannot be negative: %d", ErrInvalidConfig, c.KeepPreviousVersions)
	}

	if c.Source != nil {
		if err := c.Source.Validate(); err != nil {
			return err
		}
	}

	return nil
}

// Validate rejects a half-specified source. A partial override would
// otherwise silently fall back to the public feed for the fields that
// were left out, which reads as "my mock release is not being picked up"
// rather than as a config error.
func (c *UpdateSourceConfig) Validate() error {
	if c.APIBaseURL == "" || c.Owner == "" || c.Repo == "" {
		return fmt.Errorf("%w: update.source needs api_base_url, owner and repo", ErrInvalidConfig)
	}

	parsed, err := url.Parse(c.APIBaseURL)
	if err != nil {
		return fmt.Errorf("%w: invalid update.source.api_base_url: %w", ErrInvalidConfig, err)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%w: update.source.api_base_url needs a host: %q", ErrInvalidConfig, c.APIBaseURL)
	}
	if len(c.AllowedHosts) == 0 {
		return fmt.Errorf("%w: update.source needs allowed_hosts; downloads from anywhere else are refused", ErrInvalidConfig)
	}

	// The metadata hop chooses the download URL and says whether the
	// release carries a signature at all, and an unsigned release
	// verifies on its own checksum. Plaintext here therefore hands
	// anyone on the path the binary this node will run, so it is allowed
	// only where the hop cannot leave the machine, matching the rule the
	// downloads themselves follow.
	switch parsed.Scheme {
	case "https":
	case "http":
		if !security.IsLoopbackHost(parsed.Hostname()) {
			return fmt.Errorf("%w: update.source.api_base_url must use https for %s; plaintext is accepted only for loopback",
				ErrInvalidConfig, parsed.Hostname())
		}
	default:
		return fmt.Errorf("%w: update.source.api_base_url must be http or https, got %q", ErrInvalidConfig, parsed.Scheme)
	}

	return nil
}

// ValidateMaintenanceWindow validates a cron-style maintenance window expression
// Supported formats:
//   - "0 3 * * *"    - daily at 3:00 AM
//   - "0 3 * * 0"    - Sundays at 3:00 AM
//   - "0 3 * * 6,0"  - weekends at 3:00 AM
func ValidateMaintenanceWindow(window string) error {
	if window == "" {
		return nil
	}

	fields := strings.Fields(window)
	if len(fields) != 5 {
		return fmt.Errorf("maintenance window must have 5 fields (cron format), got %d", len(fields))
	}

	// Validate minute field (0-59)
	if err := validateCronField(fields[0], 0, 59, "minute"); err != nil {
		return err
	}

	// Validate hour field (0-23)
	if err := validateCronField(fields[1], 0, 23, "hour"); err != nil {
		return err
	}

	// Validate day of month field (1-31)
	if err := validateCronField(fields[2], 1, 31, "day of month"); err != nil {
		return err
	}

	// Validate month field (1-12)
	if err := validateCronField(fields[3], 1, 12, "month"); err != nil {
		return err
	}

	// Validate day of week field (0-6)
	if err := validateCronField(fields[4], 0, 6, "day of week"); err != nil {
		return err
	}

	return nil
}

// validateCronField validates a single cron field
func validateCronField(field string, min, max int, name string) error {
	if field == "*" {
		return nil
	}

	// Handle comma-separated values
	parts := strings.SplitSeq(field, ",")
	for part := range parts {
		part = strings.TrimSpace(part)
		if part == "*" {
			continue
		}

		// Try to parse as integer
		var val int
		if _, err := fmt.Sscanf(part, "%d", &val); err != nil {
			return fmt.Errorf("%s field contains invalid value: %q", name, part)
		}

		if val < min || val > max {
			return fmt.Errorf("%s field value %d out of range (%d-%d)", name, val, min, max)
		}
	}

	return nil
}

// ValidateVersionPin validates a version pin pattern
// Supported formats:
//   - "1.2.3"  - exact version
//   - "1.2.x"  - patch wildcard
//   - "1.x"    - minor wildcard
func ValidateVersionPin(pin string) error {
	if pin == "" {
		return nil
	}

	pin = strings.ToLower(strings.TrimSpace(pin))
	parts := strings.Split(pin, ".")

	if len(parts) < 1 || len(parts) > 3 {
		return fmt.Errorf("version pin must have 1-3 parts, got %d", len(parts))
	}

	for i, part := range parts {
		if part == "x" {
			continue
		}

		var val int
		if _, err := fmt.Sscanf(part, "%d", &val); err != nil {
			return fmt.Errorf("version pin part %d (%q) is not a number or 'x'", i+1, part)
		}

		if val < 0 {
			return fmt.Errorf("version pin part %d (%q) cannot be negative", i+1, part)
		}
	}

	return nil
}
