// Package constants provides application-wide constants and default values
package constants

import "time"

// Default Ports for various services
const (
	// DefaultZZROUTERPort is the default port for zzRouter host server
	DefaultZZROUTERPort = 9090

	// DefaultClusterPort is the default mTLS cluster listener port
	// where pairing + coord↔worker traffic lives. Operators typing a
	// bare host into `cluster pair` have this appended so they don't
	// need to remember the port number.
	DefaultClusterPort = 9091

	// DefaultOllamaPort is the default port for Ollama service
	DefaultOllamaPort = 11434

	// DefaultLlamaCppPort is the default port for llama.cpp server
	DefaultLlamaCppPort = 8080

	// DefaultVLLMPort is the default port for vLLM service
	DefaultVLLMPort = 8000

	// StandardHTTPPort is the standard HTTP port
	StandardHTTPPort = 80

	// StandardHTTPSPort is the standard HTTPS port
	StandardHTTPSPort = 443
)

// Default Intervals for various operations
const (
	// DefaultKeepAliveCheckInterval is how often to check for idle instances
	DefaultKeepAliveCheckInterval = 30 * time.Second
)

// NOTE: Timeout constants have been moved to pkg/constants/timeouts.go
// for centralized timeout management. Use constants from timeouts.go instead:
// - HTTPShortTimeout, HTTPDefaultTimeout, HTTPLongTimeout, HTTPDownloadTimeout
// - ClusterHealthCheckTimeout, ClusterQueryTimeout, ClusterActionTimeout
// - DiscoveryTimeout, VersionDiscoveryTimeout
// - HealthCheckTimeout, HealthCheckInterval, StartupTimeout
// - ProcessGracefulTimeout, ProcessForceTimeout

// Body Size Limits
const (
	// MaxClusterBodySize is the maximum allowed body size for cluster/routing responses.
	// Prevents OOM from malicious or unexpectedly large payloads (16 MB).
	MaxClusterBodySize = 16 << 20

	// MaxClusterRequestBodySize is the maximum allowed body size for incoming cluster requests (16 MB).
	MaxClusterRequestBodySize = 16 << 20
)

// Provider Modes
//
// on-demand: zzRouter launches/stops a process per model (vLLM, llama.cpp, MLX)
// service:   System daemon, zzRouter configures + restarts (Ollama via systemd/launchd)
// external:  Remote endpoint, zzRouter connects only (shared inference server)
// cloud:     Hosted provider with auth (OpenRouter, OpenAI)
// registry:  Search-only catalog, no inference (HuggingFace, Ollama library)
const (
	AppModeOnDemand = "on-demand"
	AppModeService  = "service"
	AppModeExternal = "external"
	AppModeCloud    = "cloud"
	AppModeRegistry = "registry"
)

// Keep-Alive Special Values
const (
	// KeepAliveDisabled indicates immediate unload after use
	KeepAliveDisabled = "0"

	// KeepAliveIndefinite indicates models stay loaded indefinitely
	KeepAliveIndefinite = "-1"

	// KeepAliveDefault is the default keep-alive duration
	KeepAliveDefault = "5m"
)

// Application Information
const (
	// DefaultVersion is the default version string (should be overridden by build)
	DefaultVersion = "1.0.0"

	// DefaultNodeName is the default host name when none is provided
	DefaultNodeName = "local-host"

	// CloudNodeName is the node name displayed for cloud provider models
	CloudNodeName = "cloud"
)

// Standard Port Lists
var (
	// StandardPorts are ports that don't need to be displayed in host names
	StandardPorts = []int{StandardHTTPPort, StandardHTTPSPort, DefaultZZROUTERPort}

	// CommonZZROUTERPorts are common ports to check when discovering zzRouter nodes
	CommonZZROUTERPorts = []int{DefaultZZROUTERPort, DefaultLlamaCppPort, 3000, 5000, 8000}

	// AppDefaultPorts maps app types to their default ports
	AppDefaultPorts = map[string]int{
		"ollama":    DefaultOllamaPort,
		"llama.cpp": DefaultLlamaCppPort,
		"vllm":      DefaultVLLMPort,
	}
)
