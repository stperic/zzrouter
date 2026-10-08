package modelregistry

import (
	"fmt"
	"strings"
	"sync"

	"github.com/stperic/zzrouter/pkg/config"
)

// maxLoggedEntries is the maximum number of entries in the dedup maps before they are cleared.
// This prevents unbounded memory growth while preserving dedup behavior for reasonable workloads.
const maxLoggedEntries = 1000

// Track which models we've already logged warnings/assignments for (to avoid spam)
var (
	loggedWarnings    = make(map[string]bool)
	loggedAssignments = make(map[string]bool)
	loggedMu          sync.Mutex
)

// evictLoggedMapIfFull clears the given map if it has reached maxLoggedEntries.
// Must be called while loggedMu is held.
func evictLoggedMapIfFull(m map[string]bool) {
	if len(m) >= maxLoggedEntries {
		clear(m)
	}
}

// AutoAssignProvider automatically assigns a provider to a model based on explicit model assignments or format
// Priority order: 1) Explicit model name match, 2) Format match with highest priority
// Returns the best matching provider name and host, or empty strings if no match
func AutoAssignProvider(modelName, modelFormat string, appsConfig *config.AppsConfig) (provider, host string) {
	if appsConfig == nil {
		return "", ""
	}

	// Phase 1: Check for explicit model assignments (highest priority)
	// This allows overriding format-based matching for specific models
	bestProvider := ""
	bestPriority := -1

	appsConfig.RangeApps(func(providerKey string, serviceConfig config.ServiceConfig) bool {
		if !serviceConfig.IsEnabled() || serviceConfig.Capabilities == nil {
			return true
		}
		// Check if model is explicitly assigned to this provider
		for _, assignedModel := range serviceConfig.Capabilities.Models {
			if MatchesModelPattern(modelName, assignedModel) {
				// Track highest priority match
				if serviceConfig.Capabilities.Priority > bestPriority {
					bestProvider = providerKey
					bestPriority = serviceConfig.Capabilities.Priority
				}
				break // Found match for this provider, check next provider
			}
		}
		return true
	})

	if bestProvider != "" {
		// Track assignment but don't log success (visible in list output)
		logKey := fmt.Sprintf("%s:%s:explicit", modelName, bestProvider)
		loggedMu.Lock()
		loggedAssignments[logKey] = true
		evictLoggedMapIfFull(loggedAssignments)
		loggedMu.Unlock()
		return bestProvider, "localhost"
	}

	// Phase 2: Fall back to format-based matching
	if modelFormat == "" {
		// Only log this warning once per model to avoid spam
		logKey := fmt.Sprintf("%s:no-format", modelName)
		loggedMu.Lock()
		if !loggedWarnings[logKey] {
			loggedWarnings[logKey] = true
			evictLoggedMapIfFull(loggedWarnings)
		}
		loggedMu.Unlock()
		return "", ""
	}

	// Normalize format for comparison
	format := strings.ToLower(strings.TrimSpace(modelFormat))

	// Reset for Phase 2
	bestProvider = ""
	bestPriority = -1

	// Iterate through all apps and find best match by format
	appsConfig.RangeApps(func(providerKey string, serviceConfig config.ServiceConfig) bool {
		if !serviceConfig.IsEnabled() || serviceConfig.Capabilities == nil {
			return true
		}
		// Check if provider supports this format
		supportsFormat := false
		for _, supportedFormat := range serviceConfig.Capabilities.Formats {
			if strings.ToLower(supportedFormat) == format {
				supportsFormat = true
				break
			}
		}
		if !supportsFormat {
			return true
		}
		// Check if this provider has higher priority
		if serviceConfig.Capabilities.Priority > bestPriority {
			bestProvider = providerKey
			bestPriority = serviceConfig.Capabilities.Priority
		}
		return true
	})

	if bestProvider != "" {
		// Only log once per model to avoid spam on workers
		logKey := fmt.Sprintf("%s:%s:format", modelName, bestProvider)
		loggedMu.Lock()
		if !loggedAssignments[logKey] {
			loggedAssignments[logKey] = true
			evictLoggedMapIfFull(loggedAssignments)
		}
		loggedMu.Unlock()
		return bestProvider, "localhost" // Default to localhost
	}

	// No auto-assignment - user will select provider manually if needed
	// Only log this warning once per model to avoid spam
	logKey := fmt.Sprintf("%s:%s", modelName, format)
	loggedMu.Lock()
	if !loggedWarnings[logKey] {
		loggedWarnings[logKey] = true
		evictLoggedMapIfFull(loggedWarnings)
	}
	loggedMu.Unlock()
	return "", ""
}
