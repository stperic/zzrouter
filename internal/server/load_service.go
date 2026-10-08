package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/routing"
)

// ============================================================================
// Load Service - Business Logic Layer
// ============================================================================
//
// Responsibilities:
// - Orchestrate cluster-wide model loading
// - Handle auto-discovery of model location
// - Apply business rules (force restart, etc.)
// - No HTTP concerns (that's the handler's job)
// - No direct provider calls (that's the executor's job)

// LoadService handles model loading business logic
type LoadService struct {
	cache      ModelCacheProvider
	router     routing.Router
	appsConfig func() *pkgConfig.AppsConfig
}

// NewLoadService creates a new load service
func NewLoadService(cache ModelCacheProvider, router routing.Router) *LoadService {
	return &LoadService{
		cache:  cache,
		router: router,
	}
}

// WithTierResolution lets this node resolve the provider tier tree for a
// request before it leaves for the node that will run the model. Without
// it the receiving node resolves its own tree, which is a peer-synced
// cache of this one.
func (s *LoadService) WithTierResolution(appsConfig func() *pkgConfig.AppsConfig) *LoadService {
	s.appsConfig = appsConfig
	return s
}

// launchTarget names the remote node a request would run on, or "" when
// this node would run it or the request fans out.
//
// Only a remote target is worth resolving for. Work that stays here
// reads the live tree anyway, and a fan-out has no single node to
// resolve for at all.
func (s *LoadService) launchTarget(node, model string) string {
	if s.appsConfig == nil {
		return ""
	}
	if node == "" {
		node = s.cache.TargetNodeForModel(model)
	}
	if node == "*" || s.cache.IsLocalNode(node) {
		return ""
	}
	return node
}

// preMerge resolves the tier tree for the node a request names, and
// reports the tier each value came from. Nothing is merged and nothing
// is claimed when the target is this node or unknown.
//
// Preview is the only caller. A launch is left to resolve on the node
// that runs it: what arrives in a launch request is stored as the
// caller's own request tier and replayed on every restart, so a
// coordinator that resolved a launch would pin every configured value
// against the config it read them from. Rendering a command creates no
// instance, so it carries no such consequence — and the coordinator
// owns the tree, which is what lets a preview answer for a node whose
// copy of it is a cache.
func (s *LoadService) preMerge(req *LoadModelRequest) map[string]string {
	target := s.launchTarget(req.Node, req.ModelName)
	if target == "" {
		return nil
	}
	return preMergeForRequest(s.appsConfig(), req, target)
}

// LoadModelRequest represents a request to load a model
type LoadModelRequest struct {
	requestedModel string
	Node           string `json:"node,omitempty"`
	Provider       string `json:"provider,omitempty"`
	ModelName      string `json:"model_name" binding:"required"`
	// Closed set, empty means "chat": the launch keys an instance by
	// whatever string it is handed, so an unvalidated value starts a
	// process nothing can route to.
	Endpoint    string            `json:"endpoint,omitempty" binding:"omitempty,oneof=chat embeddings reranking"`
	Force       bool              `json:"force,omitempty"`
	Parameters  map[string]string `json:"parameters,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}

// Validate validates the load model request
func (req *LoadModelRequest) Validate() error {
	if req.ModelName == "" {
		return fmt.Errorf("model_name is required")
	}
	return nil
}

// LoadModelResponse represents the response from loading a model
type LoadModelResponse struct {
	InstanceID string `json:"instance_id"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Port       int    `json:"port"`
	Status     string `json:"status"`
	Node       string `json:"node"`
	Message    string `json:"message"`
	// JobID is the pkg/jobs handle for the underlying launch. Present
	// on fresh launches AND on "already starting / already running"
	// 409 responses — retries return the SAME ID so agents can
	// subscribe idempotently.
	JobID string `json:"job_id,omitempty"`

	// Error fields (for conflict responses)
	StatusCode int    `json:"-"` // Internal use only
	Error      string `json:"error,omitempty"`
	Details    string `json:"details,omitempty"`
}

// routeForModel routes a request based on host or model-aware discovery
func (s *LoadService) routeForModel(ctx context.Context, host, model, path string, body []byte) (*routing.Response, error) {
	if host != "" && host != "*" {
		return s.router.Unicast(ctx, host, path, "POST", body)
	}
	return s.cache.RouteToModelOrBroadcast(ctx, model, path, "POST", body)
}

// LoadModel loads a model and returns the instance information
func (s *LoadService) LoadModel(ctx context.Context, req *LoadModelRequest) (*LoadModelResponse, error) {
	slog.Info("[LoadService] LoadModel", "model", req.ModelName, "provider", req.Provider, "node", req.Node, "force", req.Force)

	requestBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := s.routeForModel(ctx, req.Node, req.ModelName, "/zzrouter/v1/internal/runs/load", requestBody)
	if err != nil {
		return nil, newRoutedTransportError(err, req.Node)
	}

	// A warm-run 409 carries reusable instance data. Other failures, including
	// admission conflicts, carry a problem envelope and must remain errors.
	var loadResp LoadModelResponse
	if resp.StatusCode >= http.StatusBadRequest {
		if resp.StatusCode != http.StatusConflict || json.Unmarshal(resp.Body, &loadResp) != nil || loadResp.InstanceID == "" {
			return nil, parseRoutedError(resp.StatusCode, resp.Body)
		}
	} else if err := json.Unmarshal(resp.Body, &loadResp); err != nil {
		return nil, newRouteProblemError(http.StatusBadGateway, "Bad Gateway", "upstream returned an invalid load response", httperr.CodeUpstreamShape, resp.Node)
	}
	loadResp.StatusCode = resp.StatusCode

	// Use the host from routing response (actual IP we routed to)
	if resp.Node != "" {
		loadResp.Node = resp.Node
	}

	if resp.StatusCode == http.StatusConflict {
		slog.Info("Reusing existing instance", "model", loadResp.Model, "node", loadResp.Node, "port", loadResp.Port)
	} else {
		slog.Info("Model loaded successfully", "model", loadResp.Model, "node", loadResp.Node, "port", loadResp.Port)
	}
	return &loadResp, nil
}

// PreviewRunRequest is the same as LoadModelRequest
type PreviewRunRequest = LoadModelRequest

// PreviewRunResponse represents a run preview
type PreviewRunResponse struct {
	Model            string            `json:"model"`
	Provider         string            `json:"provider"`
	Node             string            `json:"node"`
	Port             int               `json:"port"`
	FullCommand      string            `json:"full_command"`
	Parameters       map[string]string `json:"parameters"`
	ParameterSources map[string]string `json:"parameter_sources"`
	Environment      map[string]string `json:"environment"`
	WorkingDir       string            `json:"working_dir"`
}

// PreviewRun returns a preview of what would be run without actually running it
func (s *LoadService) PreviewRun(ctx context.Context, req *PreviewRunRequest) (*PreviewRunResponse, error) {
	slog.Info("[LoadService] PreviewRun", "model", req.ModelName, "provider", req.Provider, "node", req.Node)

	sources := s.preMerge(req)

	requestBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := s.routeForModel(ctx, req.Node, req.ModelName, "/zzrouter/v1/internal/runs/preview", requestBody)
	if err != nil {
		return nil, newRoutedTransportError(err, req.Node)
	}

	// Surface worker-side errors (e.g. 400 unknown provider) as a typed
	// *RoutedError so the public controller forwards the same status +
	// Problem Details body — not a 200 with an empty preview.
	if resp.StatusCode != http.StatusOK {
		return nil, parseRoutedError(resp.StatusCode, resp.Body)
	}

	var preview PreviewRunResponse
	if err := json.Unmarshal(resp.Body, &preview); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// The node that rendered this preview received every value as a
	// request parameter, so the sources it reported read `request` for
	// all of them — true of the wire it saw, false about where the
	// values came from. Restore the tiers from the tree they were
	// actually resolved against.
	if len(sources) > 0 {
		preview.ParameterSources = sources
	}

	slog.Info("[LoadService] Preview generated for model", "model", preview.Model)
	return &preview, nil
}
