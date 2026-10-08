package version

// Capability constants for consistent capability string usage across the codebase.
// These constants fix typos like "cluster _networking" and ensure consistency.
const (
	// Core capabilities
	CapabilityCluster           = "cluster"
	CapabilityClusterUpdates    = "cluster_updates"
	CapabilityClusterNetworking = "cluster_networking"
	CapabilityClusterDiscovery  = "cluster_discovery"
	CapabilityAdminAPI          = "admin_api"
	CapabilityApps              = "providers"
	CapabilityOpenAIAPI         = "openai_api"
	CapabilityOllamaAPI         = "ollama_api"

	// Server/node capabilities
	CapabilityModelRouting            = "model_routing"
	CapabilityProviderManagement      = "provider_management"
	CapabilityAutoDiscovery           = "auto_discovery"
	CapabilityHealthMonitoring        = "health_monitoring"
	CapabilityConfigurationManagement = "configuration_management"
)

// ServerCapabilities are the capabilities exposed by zzrouter-node
var ServerCapabilities = []string{
	CapabilityCluster,
	CapabilityClusterNetworking,
	CapabilityAdminAPI,
	CapabilityApps,
	CapabilityOpenAIAPI,
	CapabilityOllamaAPI,
}

// NodeCapabilities are the capabilities exposed by zzrouter-node (detailed version)
var NodeCapabilities = []string{
	CapabilityModelRouting,
	CapabilityClusterNetworking,
	CapabilityProviderManagement,
	CapabilityAutoDiscovery,
	CapabilityHealthMonitoring,
	CapabilityConfigurationManagement,
}

// ClientCapabilities are the capabilities expected by the zzrouter client
var ClientCapabilities = []string{
	CapabilityAdminAPI,
	CapabilityProviderManagement,
	CapabilityClusterDiscovery,
}

// HealthCapabilities are the capabilities returned by health endpoints
var HealthCapabilities = []string{
	CapabilityCluster,
	CapabilityAdminAPI,
	CapabilityApps,
	CapabilityOpenAIAPI,
}

// ClusterRequiredCapabilities are the capabilities required for cluster communication
var ClusterRequiredCapabilities = []string{
	CapabilityCluster,
	CapabilityAdminAPI,
}
