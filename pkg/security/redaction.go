// Package security provides security utilities for zzRouter
package security

import (
	"os"
	"regexp"
	"strings"
)

// SensitivePatterns defines regex patterns that match sensitive data
var SensitivePatterns = []*regexp.Regexp{
	// Quoted fields also occur inside prefixed JSON and Python diagnostic text.
	regexp.MustCompile(`(?i)["'](?P<field>[a-z_][a-z0-9_-]*)["']\s*[:=]\s*"(?P<credential>(?:\\.|[^"\\\r\n])*)"`),
	regexp.MustCompile(`(?i)["'](?P<field>[a-z_][a-z0-9_-]*)["']\s*[:=]\s*'(?P<credential>(?:\\.|[^'\\\r\n])*)'`),
	// API keys with common prefixes
	regexp.MustCompile(`(?i)(mx-[a-zA-Z0-9_-]{20,})`),                        // zzRouter node keys
	regexp.MustCompile(`(zzr_[a-zA-Z0-9_-]{20,})`),                           // zzRouter virtual keys (pkg/access/keys.KeyPrefix)
	regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,})`),                          // OpenAI-style keys
	regexp.MustCompile(`(hf_[a-zA-Z0-9]{20,})`),                              // HuggingFace tokens
	regexp.MustCompile(`(?i)(api[_-]?key\s*[:=]\s*)([a-zA-Z0-9_-]{16,})`),    // Generic API keys
	regexp.MustCompile(`(?i)(bearer\s+)([a-zA-Z0-9._-]{20,})`),               // Bearer tokens
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)([a-zA-Z0-9._-]{20,})`), // Auth headers
	regexp.MustCompile(`(?i)(password\s*[:=]\s*)([^\s,}"']{4,})`),            // Passwords
	regexp.MustCompile(`(?i)(secret\s*[:=]\s*)([^\s,}"']{8,})`),              // Secrets
	regexp.MustCompile(`(?i)(token\s*[:=]\s*)([a-zA-Z0-9._-]{16,})`),         // Tokens
	regexp.MustCompile(`(?i)(ZZROUTER_[A-Z_]*KEY\s*=\s*)([^\s]{8,})`),        // zzRouter env vars
	regexp.MustCompile(`(?i)(X-API-Key\s*[:=]\s*)([^\s,}"']{8,})`),           // X-API-Key header
	regexp.MustCompile(`(?i)(X-Admin-API-Key\s*[:=]\s*)([^\s,}"']{8,})`),     // Admin key header
	regexp.MustCompile(`(?i)(X-User-API-Key\s*[:=]\s*)([^\s,}"']{8,})`),      // User key header
	regexp.MustCompile(`(?i)(X-Cluster-API-Key\s*[:=]\s*)([^\s,}"']{8,})`),   // Cluster key header
}

// RedactedPlaceholder is the string used to replace sensitive data
const RedactedPlaceholder = "[REDACTED]"

// RedactSensitive replaces sensitive data in a string with [REDACTED]
// This should be called before logging any user-provided or system data
func RedactSensitive(s string) string {
	result := s

	for _, pattern := range SensitivePatterns {
		// For patterns with capture groups, replace only the sensitive part
		result = pattern.ReplaceAllStringFunc(result, func(match string) string {
			if group := pattern.SubexpIndex("credential"); group > 0 {
				positions := pattern.FindStringSubmatchIndex(match)
				keyGroup := pattern.SubexpIndex("field")
				key := match[positions[2*keyGroup]:positions[2*keyGroup+1]]
				if !isJSONSensitiveKey(key) {
					return match
				}
				return match[:positions[2*group]] + RedactedPlaceholder + match[positions[2*group+1]:]
			}
			// Find where the sensitive value starts (after = or : or space)
			for _, sep := range []string{"= ", "=", ": ", ":", " "} {
				if idx := strings.Index(match, sep); idx != -1 {
					prefix := match[:idx+len(sep)]
					return prefix + RedactedPlaceholder
				}
			}
			// If no separator found, redact the whole match
			return RedactedPlaceholder
		})
	}

	return result
}

// RedactMap redacts sensitive values in a map (useful for logging headers/env vars)
func RedactMap(m map[string]string) map[string]string {
	result := make(map[string]string, len(m))
	for k, v := range m {
		if IsSensitiveKey(k) {
			result[k] = RedactedPlaceholder
		} else {
			result[k] = v
		}
	}
	return result
}

// SensitiveKeyPatterns are key names that indicate sensitive values
var SensitiveKeyPatterns = []string{
	"key",
	"secret",
	"password",
	"passwd",
	"token",
	"auth",
	"credential",
	"api_key",
	"apikey",
	"bearer",
}

// IsSensitiveKey checks if a key name suggests a sensitive value
func IsSensitiveKey(key string) bool {
	lowerKey := strings.ToLower(key)
	for _, pattern := range SensitiveKeyPatterns {
		if strings.Contains(lowerKey, pattern) {
			return true
		}
	}
	return false
}

// AllowedEnvVars is the whitelist of environment variables that can be expanded
// in command arguments. This prevents accidental exposure of sensitive env vars.
var AllowedEnvVars = map[string]bool{
	// System paths
	"HOME":            true,
	"USER":            true,
	"PATH":            true,
	"PWD":             true,
	"TMPDIR":          true,
	"TMP":             true,
	"TEMP":            true,
	"XDG_CONFIG_HOME": true,
	"XDG_DATA_HOME":   true,
	"XDG_CACHE_HOME":  true,

	// zzRouter non-sensitive config
	"ZZROUTER_CONFIG_DIR": true,
	"ZZROUTER_LOG_DIR":    true,
	"ZZROUTER_MODEL_DIR":  true,
	"ZZROUTER_DEBUG":      true,
	"ZZROUTER_ENV":        true,

	// GPU/CUDA
	"CUDA_VISIBLE_DEVICES":   true,
	"CUDA_HOME":              true,
	"NVIDIA_VISIBLE_DEVICES": true,

	// Python
	"PYTHONPATH":        true,
	"VIRTUAL_ENV":       true,
	"CONDA_PREFIX":      true,
	"CONDA_DEFAULT_ENV": true,

	// Model paths
	"HF_HOME":               true,
	"HUGGINGFACE_HUB_CACHE": true,
	"TRANSFORMERS_CACHE":    true,
	"OLLAMA_MODELS":         true,

	// MLX
	"MLX_MODELS": true,
}

// ExpandEnvWhitelisted expands only whitelisted environment variables in a string
// Variables not in the whitelist are left unexpanded
// This prevents accidental exposure of sensitive env vars like API keys
func ExpandEnvWhitelisted(s string) string {
	return os.Expand(s, func(key string) string {
		if AllowedEnvVars[key] {
			return os.Getenv(key)
		}
		// Return the original ${VAR} or $VAR syntax unexpanded
		return "${" + key + "}"
	})
}
