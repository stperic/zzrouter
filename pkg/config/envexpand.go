package config

import (
	"os"
	"strings"
)

// ExpandEnv expands ${VAR} and ${VAR:-default} patterns in s.
// If VAR is set and non-empty, its value is used.
// If VAR is unset or empty and a default is provided, the default is used.
// If VAR is unset or empty with no default, it expands to "".
// Uses os.Expand with a custom mapping, avoiding regex entirely.
func ExpandEnv(s string) string {
	return os.Expand(s, func(key string) string {
		name, defaultVal, hasDefault := parseVarWithDefault(key)
		if val := os.Getenv(name); val != "" {
			return val
		}
		if hasDefault {
			return defaultVal
		}
		return ""
	})
}

// NormalizeEndpoint expands environment variables in an endpoint template and
// strips trailing "/" and "/v1". Azure documents endpoints with "/v1/" included
// (e.g., https://...azure.com/openai/v1/) but zzRouter's request paths already
// include "/v1/", so we normalize to avoid double "/v1/v1/" in proxied URLs.
func NormalizeEndpoint(endpoint string) string {
	endpoint = strings.TrimRight(ExpandEnv(endpoint), "/")
	endpoint = strings.TrimSuffix(endpoint, "/v1")
	return endpoint
}

// parseVarWithDefault splits "VAR:-default" into ("VAR", "default", true).
// For plain "VAR" it returns ("VAR", "", false).
func parseVarWithDefault(key string) (name, defaultVal string, hasDefault bool) {
	if before, after, ok := strings.Cut(key, ":-"); ok {
		return before, after, true
	}
	return key, "", false
}
