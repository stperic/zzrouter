// Package config provides OS-agnostic configuration file management for zzRouter
// ServiceConfig methods for app configuration
package config

import (
	"github.com/stperic/zzrouter/pkg/constants"
)

// GetParameters returns app-level parameters
func (s *ServiceConfig) GetParameters() map[string]string {
	if s.Defaults != nil && s.Defaults.Parameters != nil {
		return s.Defaults.Parameters
	}
	return make(map[string]string)
}

// GetEnvironment returns app-level environment variables
func (s *ServiceConfig) GetEnvironment() map[string]string {
	if s.Defaults != nil && s.Defaults.Environment != nil {
		return s.Defaults.Environment
	}
	return make(map[string]string)
}

// GetMetadata returns the app's behavioral metadata, or nil if unset.
func (s *ServiceConfig) GetMetadata() *AppMetadata {
	if s.Defaults == nil {
		return nil
	}
	return s.Defaults.Metadata
}

// GetShutdownTimeout returns the configured shutdown timeout hint.
func (s *ServiceConfig) GetShutdownTimeout() string {
	if m := s.GetMetadata(); m != nil {
		return m.ShutdownTimeout
	}
	return ""
}

// GetProcessorType returns the display processor type (e.g., "GPU", "Metal GPU").
func (s *ServiceConfig) GetProcessorType() string {
	if m := s.GetMetadata(); m != nil {
		return m.ProcessorType
	}
	return ""
}

// GetContextParam returns the parameter name used for context length.
func (s *ServiceConfig) GetContextParam() string {
	if m := s.GetMetadata(); m != nil {
		return m.ContextParam
	}
	return ""
}

// IsEnabled returns true if the app is explicitly enabled on the node
// this view is read for (see EnabledOn).
// Three-state logic:
//   - nil (not set):  Returns false (use for auto-discovery)
//   - &true:          Returns true (explicitly enabled)
//   - &false:         Returns false (explicitly disabled)
func (s *ServiceConfig) IsEnabled() bool {
	enabled := s.EnabledOn(s.node)
	return enabled != nil && *enabled
}

// SetEnabled sets the enabled state explicitly (not nil).
//
// ARCHITECTURE: Runtime code MUST NOT call this directly. Route through
// (*Server).FinalizeOnboarding / FinalizeOffboarding (internal/server),
// which funnel through AppsConfigStore.SetProviderEnabled so the listener
// chain reconciles port pools, protocol registry, and local caches.
// Direct calls to SetEnabled bypass that chain and silently desync
// in-memory state from on-disk config.
//
// Legitimate callers (enforced by TestServiceConfigSetEnabledAllowlist):
//   - AppsConfigStore itself (store mutation path is the owner)
//   - Init-time seeding (CLI init, DiscoverAndAutoEnable) — runs before
//     the server has listeners registered, so the chain is moot
func (s *ServiceConfig) SetEnabled(enabled bool) {
	s.Enabled = &enabled
}

// EnabledOn is the enabled value node runs with: its own nodes entry,
// else the provider's top-level value. Readers that speak for another
// node, such as the coordinator's catalog, ask for that node.
func (s *ServiceConfig) EnabledOn(node string) *bool {
	if spec, ok := s.Nodes[node]; ok && spec.Enabled != nil {
		return spec.Enabled
	}
	return s.Enabled
}

// SyncedToWorkers reports whether every worker's copy of the provider is the
// coordinator's, so a node's enable is its entry there. Cloud providers and
// registries never sync: each node owns its copy and its top-level enable.
func (s *ServiceConfig) SyncedToWorkers() bool {
	return !s.IsCloudProvider() && !s.IsModelHubRegistry()
}

// SetEnabledOn records node's enable choice and reports whether it
// changes what node runs with. Without a node tier, or for an empty
// node, it sets the top-level value. Same caller rule as SetEnabled.
func (s *ServiceConfig) SetEnabledOn(node string, enabled bool) bool {
	if current := s.EnabledOn(node); current != nil && *current == enabled {
		return false
	}
	if node == "" || !s.SyncedToWorkers() {
		s.Enabled = &enabled
		return true
	}
	spec := ensureNodeSpec(s, node)
	spec.Enabled = &enabled
	s.Nodes[node] = spec
	return true
}

// disabledEverywhere reports whether no node runs the provider: the
// default is explicitly off and no node entry turns it on.
func disabledEverywhere(enabled *bool, nodes map[string]NodeSpec) bool {
	if enabled == nil || *enabled {
		return false
	}
	for _, spec := range nodes {
		if spec.Enabled != nil && *spec.Enabled {
			return false
		}
	}
	return true
}

// IsExplicitlyDisabled returns true only if enabled: false is set
// Use this to skip apps - allows auto-discovery for nil values
// Three-state logic:
//   - nil (not set):  Returns false (allow auto-discovery)
//   - &true:          Returns false (explicitly enabled)
//   - &false:         Returns true (explicitly disabled - SKIP)
func (s *ServiceConfig) IsExplicitlyDisabled() bool {
	enabled := s.EnabledOn(s.node)
	return enabled != nil && !*enabled
}

// IsAutoDiscovery returns true if the app is in auto-discovery mode (enabled: nil)
// Auto-discovery apps are registered but enabled based on runtime detection
func (s *ServiceConfig) IsAutoDiscovery() bool {
	return s.EnabledOn(s.node) == nil
}

// ============================================================================
// Mode Semantic Helpers
// ============================================================================
// Use these instead of raw string comparisons against Mode.
// They encode the behavioral contracts of each mode.

// LaunchesProcess returns true if zzRouter starts/stops a process per model.
// True for: on-demand. False for: service, external, cloud.
func (s *ServiceConfig) LaunchesProcess() bool {
	return s.Mode == constants.AppModeOnDemand
}

// HasEndpoint returns true if the app has its own running endpoint
// that zzRouter connects to (rather than launching a process).
// True for: service, external, cloud. False for: on-demand.
func (s *ServiceConfig) HasEndpoint() bool {
	return s.Mode == constants.AppModeService ||
		s.Mode == constants.AppModeExternal ||
		s.Mode == constants.AppModeCloud
}

// API returns what the provider's endpoint is sent to authenticate
// (runtime.api), or nil when it declares nothing.
func (s *ServiceConfig) API() *APIConfig {
	if s == nil || s.Runtime == nil {
		return nil
	}
	return s.Runtime.API
}

// IsCloudProvider returns true if the app is a hosted cloud API.
func (s *ServiceConfig) IsCloudProvider() bool {
	return s.Mode == constants.AppModeCloud
}

// IsCloudAvailable returns true if the cloud app is enabled in config AND has
// resolvable API credentials (env vars set). Both conditions must be true.
func (s *ServiceConfig) IsCloudAvailable() bool {
	if !s.IsCloudProvider() || !s.IsEnabled() {
		return false
	}
	if s.Runtime == nil || s.Runtime.API == nil {
		return false
	}
	return s.Runtime.API.HasCredentials()
}

// IsLocalProvider returns true if the app is a remote endpoint
// that zzRouter connects to but cannot configure or restart.
func (s *ServiceConfig) IsLocalProvider() bool {
	return s.Mode == constants.AppModeExternal
}

// IsModelHubRegistry returns true if the entry is a search-only catalog (no inference).
// Registry entries (e.g., HuggingFace) provide model search but are not providers.
func (s *ServiceConfig) IsModelHubRegistry() bool {
	return s.Mode == constants.AppModeRegistry
}

// GetAppProtocol returns the app protocol
func (s *ServiceConfig) GetAppProtocol() AppProtocol {
	if s.Protocol != "" {
		return s.Protocol
	}
	return ProtocolOpenAI // Default
}
