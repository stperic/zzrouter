package runsettings

import (
	"fmt"
	"strings"
	"time"
)

// Reserved run parameters belong to zzRouter, never to an engine's command line.
const (
	KeepAliveParameter = "keep-alive"
	EvictIdleParameter = "evict-idle-on-memory-pressure"
)

// Settings is one launch's idle and memory-pressure policy.
type Settings struct {
	KeepAlive         time.Duration
	KeepAliveExplicit bool
	EvictIdle         bool
	EvictIdleExplicit bool
}

// Parse validates reserved parameters, with the legacy node eviction option as fallback.
func Parse(params map[string]string, evictDefault bool) (Settings, error) {
	for key := range params {
		if IsParameter(key) && key != KeepAliveParameter && key != EvictIdleParameter {
			return Settings{}, fmt.Errorf("use canonical run parameter spelling for %q", key)
		}
	}
	raw, explicit := params[KeepAliveParameter]
	duration, err := Duration(raw)
	if err != nil {
		return Settings{}, err
	}
	s := Settings{KeepAlive: duration, KeepAliveExplicit: explicit, EvictIdle: evictDefault}
	if raw, ok := params[EvictIdleParameter]; ok {
		if raw != "true" && raw != "false" {
			return Settings{}, fmt.Errorf("%s must be true or false", EvictIdleParameter)
		}
		s.EvictIdle, s.EvictIdleExplicit = raw == "true", true
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

// IsParameter recognizes reserved names, including underscore aliases engines normalize to flags.
func IsParameter(key string) bool {
	key = strings.ReplaceAll(key, "_", "-")
	return key == KeepAliveParameter || key == EvictIdleParameter
}
