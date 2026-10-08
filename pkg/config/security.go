// Package config provides OS-agnostic configuration file management for zzRouter
// Security utilities for API key management
package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// GetAPIKey returns the API key for a host connection, with smart resolution
func (hc *NodeConnection) GetAPIKey() (string, error) {
	// Priority 1: Key field (smart resolution)
	if hc.Key != "" {
		return resolveAPIKey(hc.Key, "ZZROUTER_CLIENT_KEY")
	}

	// Priority 2: Direct APIKey field (deprecated)
	if hc.APIKey != "" {
		return hc.APIKey, nil
	}

	return "", fmt.Errorf("no API key configured: set key in host connection")
}

// GetAdminAPIKey returns the admin API key from auth configuration
func (ac *AuthConfig) GetAdminAPIKey() (string, error) {
	if ac.AdminKey != "" {
		return resolveAPIKey(ac.AdminKey, "ZZROUTER_ADMIN_API_KEY")
	}

	// Check default environment variable
	if key := os.Getenv("ZZROUTER_ADMIN_API_KEY"); key != "" {
		return key, nil
	}

	return "", fmt.Errorf("no admin API key configured: set auth.admin_key or ZZROUTER_ADMIN_API_KEY environment variable")
}

// GetClusterNetworkKey resolves the cluster-network key from auth.cluster_network_key
// or the ZZROUTER_CLUSTER_NETWORK_KEY env var. Empty string means unset; the
// worker compat-gate treats that as deny-all (so misconfigured workers don't
// silently expose /v1/* without cluster-only auth).
func (ac *AuthConfig) GetClusterNetworkKey() string {
	if ac.ClusterNetworkKey != "" {
		if key, err := resolveAPIKey(ac.ClusterNetworkKey, "ZZROUTER_CLUSTER_NETWORK_KEY"); err == nil {
			return key
		}
	}
	return os.Getenv("ZZROUTER_CLUSTER_NETWORK_KEY")
}

// GetUserAPIKey returns the user API key from auth configuration
// SECURITY: Does not fallback to admin key - each role requires explicit configuration
func (ac *AuthConfig) GetUserAPIKey() (string, error) {
	if ac.UserKey != "" {
		return resolveAPIKey(ac.UserKey, "ZZROUTER_API_KEY")
	}

	// Check default environment variable
	if key := os.Getenv("ZZROUTER_API_KEY"); key != "" {
		return key, nil
	}

	// SECURITY FIX: Removed fallback to admin key
	// Each role must be explicitly configured to prevent privilege escalation
	// If only admin access is needed, clients should use the admin key directly
	return "", fmt.Errorf("no user API key configured: set auth.user_key or ZZROUTER_API_KEY environment variable")
}

// resolveAPIKey intelligently resolves an API key from a string value:
// - If it looks like an environment variable name (UPPERCASE_SNAKE_CASE), resolve from env
// - Otherwise, treat it as a literal value (for development)
func resolveAPIKey(value string, defaultEnvVar string) (string, error) {
	// Check if value looks like an environment variable name
	if isEnvVarName(value) {
		// It's an env var reference - resolve it
		key := os.Getenv(value)
		if key == "" {
			// Try default env var as fallback
			if defaultEnvVar != "" {
				if fallbackKey := os.Getenv(defaultEnvVar); fallbackKey != "" {
					return fallbackKey, nil
				}
			}
			return "", fmt.Errorf("%w: %s", ErrMissingEnvKey, value)
		}
		return key, nil
	}

	// It's a literal value - use directly (for development)
	return value, nil
}

// isEnvVarName checks if a string looks like an environment variable name
// Environment variable names typically:
// - Are UPPERCASE
// - Use underscores for word separation
// - Start with a letter
// - Contain only letters, numbers, and underscores
func isEnvVarName(s string) bool {
	if s == "" {
		return false
	}

	// Must start with uppercase letter or underscore
	if !isUppercaseLetter(s[0]) && s[0] != '_' {
		return false
	}

	// Check if it's all uppercase with underscores and numbers
	hasUppercase := false
	for _, ch := range s {
		if isUppercaseLetter(byte(ch)) {
			hasUppercase = true
		} else if ch != '_' && !isDigit(byte(ch)) {
			// Contains lowercase or special chars - not an env var name
			return false
		}
	}

	// Must have at least one uppercase letter
	// This prevents things like "123" or "___" from being treated as env vars
	return hasUppercase
}

// isUppercaseLetter checks if a byte is an uppercase letter (A-Z)
func isUppercaseLetter(b byte) bool {
	return b >= 'A' && b <= 'Z'
}

// isDigit checks if a byte is a digit (0-9)
func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// MinAPIKeyLength is the minimum required length for API keys in production
// SECURITY: 32 characters provides sufficient entropy for cryptographic security
const MinAPIKeyLength = 32

// ValidateConfiguredKeys runs ValidateAPIKey against every static API
// key the node has configured. Missing keys are not errors here; only
// configured keys must meet the strength bar.
func ValidateConfiguredKeys(cfg *NodeConfig) error {
	if k, err := cfg.Auth.GetAdminAPIKey(); err == nil {
		if verr := ValidateAPIKey(k); verr != nil {
			return fmt.Errorf("admin API key rejected: %w", verr)
		}
	}
	if k, err := cfg.Auth.GetUserAPIKey(); err == nil {
		if verr := ValidateAPIKey(k); verr != nil {
			return fmt.Errorf("user API key rejected: %w", verr)
		}
	}
	return nil
}

// ValidateAPIKey validates an API key for security requirements
// SECURITY: Enforces minimum length and entropy requirements
func ValidateAPIKey(key string) error {
	if key == "" {
		return fmt.Errorf("API key cannot be empty")
	}

	// SECURITY: Require minimum 32 characters for production security
	// This ensures sufficient entropy to resist brute-force attacks
	if len(key) < MinAPIKeyLength {
		return fmt.Errorf("API key too short (minimum %d characters, got %d) - use 'zzrouter admin generate-key' to create a secure key", MinAPIKeyLength, len(key))
	}

	// Check for obviously weak keys
	weakKeys := []string{
		"password",
		"test",
		"default",
		"changeme",
		"secret",
		"admin",
		"123456",
		"qwerty",
	}

	lowerKey := strings.ToLower(key)
	for _, weak := range weakKeys {
		if strings.Contains(lowerKey, weak) {
			return fmt.Errorf("API key contains weak pattern: %s", weak)
		}
	}

	// SECURITY: Check for basic entropy - key should have variety
	// A key with all same characters or very low entropy is suspicious
	if hasLowEntropy(key) {
		return fmt.Errorf("API key has insufficient entropy - use 'zzrouter admin generate-key' to create a secure key")
	}

	return nil
}

// hasLowEntropy checks if a string has suspiciously low entropy
// Returns true if the key appears to be weak (repeated characters, sequences)
func hasLowEntropy(key string) bool {
	if len(key) == 0 {
		return true
	}

	// Count unique characters
	uniqueChars := make(map[rune]int)
	for _, ch := range key {
		uniqueChars[ch]++
	}

	// If less than 10 unique characters in a 32+ char key, it's suspicious
	// A truly random base64 key should have many unique characters
	if len(uniqueChars) < 10 {
		return true
	}

	// Check if any single character makes up more than 25% of the key
	threshold := len(key) / 4
	for _, count := range uniqueChars {
		if count > threshold {
			return true
		}
	}

	return false
}

// GenerateAPIKey generates a cryptographically secure random API key
func GenerateAPIKey() (string, error) {
	// Generate 32 random bytes (256 bits) for strong security
	bytes := make([]byte, 32)
	_, err := rand.Read(bytes)
	if err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Encode as base64 URL-safe string without padding
	key := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(bytes)

	// Add mx- prefix for leak detection and identification
	finalKey := "mx-" + key

	// Validate the generated key meets security requirements
	if err := ValidateAPIKey(finalKey); err != nil {
		// This should never happen with proper generation, but just in case
		return "", fmt.Errorf("generated key failed validation: %w", err)
	}

	return finalKey, nil
}
