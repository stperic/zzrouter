package config

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/config/runsettings"
)

// Run parameter names and their parser are shared with provider schema validation.
const (
	KeepAliveParameter = runsettings.KeepAliveParameter
	EvictIdleParameter = runsettings.EvictIdleParameter
)

// RunSettings is one launch's idle and memory-pressure policy.
type RunSettings = runsettings.Settings

// ParseRunSettings validates reserved parameters with the legacy node eviction option as fallback.
func ParseRunSettings(params map[string]string, evictDefault bool) (RunSettings, error) {
	return runsettings.Parse(params, evictDefault)
}

// ValidateRunSettings checks every site, including lower tiers hidden by an override.
func (s ServiceConfig) ValidateRunSettings() error {
	for _, site := range s.ParameterSites() {
		flags := make(map[string]ResolvedValue, len(site.Parameters))
		LayParameters(flags, site.Parameters, ResolvedValue{})
		if _, err := ParseRunSettings(FlattenParameters(flags), false); err != nil {
			return fmt.Errorf("provider %q %s: %w", s.Name, site.Path, err)
		}
	}
	return nil
}
