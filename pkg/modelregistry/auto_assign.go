package modelregistry

import (
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
)

// AutoAssignProvider prefers explicit model assignments, then architecture-filtered
// format matches by priority. An unmatched model returns empty provider and host.
func AutoAssignProvider(modelName, modelFormat, architecture string, appsConfig *config.AppsConfig) (provider, host string) {
	if appsConfig == nil {
		return "", ""
	}

	// Operator intent takes precedence over architecture declarations.
	bestProvider := ""
	bestPriority := -1

	appsConfig.RangeApps(func(providerKey string, serviceConfig config.ServiceConfig) bool {
		if !serviceConfig.IsEnabled() || serviceConfig.Capabilities == nil {
			return true
		}
		for _, assignedModel := range serviceConfig.Capabilities.Models {
			if MatchesModelPattern(modelName, assignedModel) {
				if serviceConfig.Capabilities.Priority > bestPriority {
					bestProvider = providerKey
					bestPriority = serviceConfig.Capabilities.Priority
				}
				break
			}
		}
		return true
	})

	if bestProvider != "" {
		return bestProvider, "localhost"
	}

	if modelFormat == "" {
		return "", ""
	}

	format := strings.ToLower(strings.TrimSpace(modelFormat))

	bestProvider = ""
	bestPriority = -1

	candidates := map[string][]string{}
	appsConfig.RangeApps(func(name string, svc config.ServiceConfig) bool {
		if svc.IsEnabled() && svc.Capabilities != nil && FormatSupported(svc.Capabilities.Formats, format) {
			candidates[name] = svc.Capabilities.Architectures
		}
		return true
	})
	for _, name := range config.FilterArchitectures(candidates, architecture, false) {
		svc, _ := appsConfig.LookupApp(name)
		if svc.Capabilities.Priority > bestPriority {
			bestProvider, bestPriority = name, svc.Capabilities.Priority
		}
	}

	if bestProvider != "" {
		return bestProvider, "localhost" // Default to localhost
	}

	return "", ""
}
