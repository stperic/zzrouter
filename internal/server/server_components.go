// package server provides HTTP handlers for the zzrouter host server.
// Node components - grouped service and controller dependencies

package server

// Services groups all business logic services used by Server
// This reduces the Server field count and improves organization
type Services struct {
	Runs         *RunsService         // Runs lifecycle management
	RunsUtility  *RunsUtilityService  // Runs local utility queries
	Load         *LoadService         // Model loading operations
	Deployments  *DeploymentsService  // Model placement (deploy) operations
	Apps         *AppsService         // Apps management
	Nodes        *NodesService        // Nodes management
	System       *SystemService       // System information
	Discovery    *DiscoveryService    // Resource discovery
	Model        *ModelService        // Model operations
	Search       *ModelSearchService  // Model search (HuggingFace / Ollama / cloud)
	Ollama       OllamaService        // Ollama API operations
	InferenceLog *InferenceLogService // Inference log query/stream
	Keys         *KeysService         // Virtual key management
	Teams        *TeamsService        // Team management
}

// Controllers groups all HTTP controllers used by Server
// Controllers handle HTTP request parsing and delegate to services
type Controllers struct {
	Models         *ModelsController         // Models endpoints
	Runs           *RunsController           // Runs endpoints
	Discovery      *DiscoveryController      // Discovery endpoints
	Deployments    *DeploymentsController    // Deployments endpoints
	Nodes          *NodesController          // Nodes endpoints
	System         *SystemController         // System endpoints
	Params         *ParamsController         // Parameter management endpoints
	Assets         *ProviderAssetsController // Provider asset files
	InferenceLog   *InferenceLogController   // Inference log endpoints
	ModelGroups    *ModelGroupsController    // Model group endpoints
	Keys           *KeysController           // Virtual key management endpoints
	Teams          *TeamsController          // Team management endpoints
	ProviderStatus *ProviderStatusController // Provider health/cooldown status
}
