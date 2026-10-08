// Package constants provides application-wide constants and default values
package constants

// =============================================================================
// API Path Constants - Centralized route paths for the application
// =============================================================================
// This file is the SINGLE SOURCE OF TRUTH for all API path strings.
// Use these constants instead of hardcoding path strings in your code.
// =============================================================================

// -----------------------------------------------------------------------------
// OpenAI Compatibility API Paths
// -----------------------------------------------------------------------------

const (
	// OpenAI v1 API paths (compatible with OpenAI SDK)
	OpenAIV1ChatCompletions = "/v1/chat/completions"
	OpenAIV1Completions     = "/v1/completions"
	OpenAIV1Models          = "/v1/models"
	OpenAIV1Embeddings      = "/v1/embeddings"
)

// -----------------------------------------------------------------------------
// zzRouter Public API Paths
// -----------------------------------------------------------------------------

const (
	// zzRouter native API base path
	ZZROUTERAPIBase = "/zzrouter/v1"

	// Model management
	ZZROUTERModels     = "/zzrouter/v1/models"
	ZZROUTERModelsLoad = "/zzrouter/v1/models/load"

	// Instance management (runs)
	ZZROUTERRuns = "/zzrouter/v1/runs"

	// App management
	ZZROUTERApps = "/zzrouter/v1/providers"

	// Deploy/download management
	ZZROUTERDeployments = "/zzrouter/v1/deployments"

	// Search
	ZZROUTERSearch = "/zzrouter/v1/search"

	// Nodes
	ZZROUTERNodes = "/zzrouter/v1/nodes"

	// Cluster
	ZZROUTERCluster = "/zzrouter/v1/cluster"

	// Discovery
	ZZROUTERDiscovery = "/zzrouter/v1/discovery"

	// System
	ZZROUTERSystem = "/zzrouter/v1/system"
)

// -----------------------------------------------------------------------------
// zzRouter Internal API Paths (Cluster Communication)
// -----------------------------------------------------------------------------

const (
	// Internal API base path (cluster-to-cluster communication)
	ZZROUTERInternalBase = "/zzrouter/v1/internal"

	// Internal runs management
	ZZROUTERInternalRuns        = "/zzrouter/v1/internal/runs"
	ZZROUTERInternalRunsStop    = "/zzrouter/v1/internal/runs/stop"
	ZZROUTERInternalRunsRestart = "/zzrouter/v1/internal/runs/restart"

	// Internal models
	ZZROUTERInternalModels     = "/zzrouter/v1/internal/models"
	ZZROUTERInternalModelsLoad = "/zzrouter/v1/internal/models/load"

	// Internal sync
	ZZROUTERInternalSync       = "/zzrouter/v1/internal/sync"
	ZZROUTERInternalSyncModels = "/zzrouter/v1/internal/sync/models"
	ZZROUTERInternalSyncFiles  = "/zzrouter/v1/internal/sync/files"

	// Internal apps
	ZZROUTERInternalApps = "/zzrouter/v1/internal/providers"

	// Internal deployments
	ZZROUTERInternalDeployments = "/zzrouter/v1/internal/deployments"
)

// -----------------------------------------------------------------------------
// Health Check Paths
// -----------------------------------------------------------------------------

const (
	// Health check endpoints (no auth required)
	HealthLive  = "/health/live"
	HealthReady = "/health/ready"
	HealthFull  = "/health"
)
