// Package runsettings owns the run-parameter key vocabulary: how a key is
// spelled (FlagName), the auto value, and the run settings zzRouter reserves.
package runsettings

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Reserved run parameters belong to zzRouter, never to an engine's command line.
const (
	KeepAliveParameter       = "keep-alive"
	EvictIdleParameter       = "evict-idle-on-memory-pressure"
	MemoryAdmissionParameter = "memory-admission"

	MemoryAdmissionEnforce = "enforce"
	MemoryAdmissionReport  = "report"
)

// Auto is the reserved value asking zzRouter to choose a parameter's value.
const Auto = "auto"

var paramKeyRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// ValidKey reports whether a parameter key uses the run-parameter vocabulary.
func ValidKey(key string) bool {
	return paramKeyRe.MatchString(key)
}

// InvalidKeyError describes a key outside the run-parameter vocabulary.
func InvalidKeyError(key string) error {
	return fmt.Errorf("invalid parameter key %q: must start with a letter and contain only letters, digits, hyphens, underscores", key)
}

// FlagName is how argv spells a parameter key, after "--": parameter_name
// and parameter-name are one flag, which every merge, write and check keys by.
func FlagName(key string) string {
	return strings.ReplaceAll(key, "_", "-")
}

// Settings is one launch's idle and memory-pressure policy.
type Settings struct {
	KeepAlive         time.Duration
	KeepAliveExplicit bool
	EvictIdle         bool
	EvictIdleExplicit bool
	MemoryAdmission   string
}

// Parse validates reserved parameters keyed by flag name, with the legacy
// node eviction option as fallback.
func Parse(params map[string]string, evictDefault bool) (Settings, error) {
	raw, explicit := params[KeepAliveParameter]
	duration, err := Duration(raw)
	if err != nil {
		return Settings{}, err
	}
	s := Settings{KeepAlive: duration, KeepAliveExplicit: explicit, EvictIdle: evictDefault, MemoryAdmission: MemoryAdmissionEnforce}
	if raw, ok := params[EvictIdleParameter]; ok {
		if raw != "true" && raw != "false" {
			return Settings{}, fmt.Errorf("%s must be true or false", EvictIdleParameter)
		}
		s.EvictIdle, s.EvictIdleExplicit = raw == "true", true
	}
	if raw, ok := params[MemoryAdmissionParameter]; ok {
		if raw != MemoryAdmissionEnforce && raw != MemoryAdmissionReport {
			return Settings{}, fmt.Errorf("%s must be %s or %s", MemoryAdmissionParameter, MemoryAdmissionEnforce, MemoryAdmissionReport)
		}
		s.MemoryAdmission = raw
	}
	return s, nil
}

// Duration parses the configured idle limit, preserving empty/default and pin semantics.
func Duration(raw string) (time.Duration, error) {
	switch raw {
	case "":
		return 5 * time.Minute, nil
	case "0":
		return 0, nil
	case "-1":
		return -1, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid keep_alive duration %q: %w", raw, err)
	}
	return d, nil
}

// IsParameter recognizes a reserved name, keyed by flag name.
func IsParameter(key string) bool {
	return key == KeepAliveParameter || key == EvictIdleParameter || key == MemoryAdmissionParameter
}
