package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
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

// WithAppsConfig lets the service refuse a run target the provider config
// rules out before routing it. Tiers resolve on the node that runs the
// model, for a preview as for a start.
func (s *LoadService) WithAppsConfig(appsConfig func() *pkgConfig.AppsConfig) *LoadService {
	s.appsConfig = appsConfig
	return s
}

// LoadModelRequest represents a request to load a model
type LoadModelRequest struct {
	Node      string `json:"node,omitempty"`
	Provider  string `json:"provider,omitempty"`
	ModelName string `json:"model_name,omitempty"`
	// Closed set, empty means "chat": the launch keys an instance by
	// whatever string it is handed, so an unvalidated value starts a
	// process nothing can route to.
	Endpoint    string                 `json:"endpoint,omitempty" binding:"omitempty,oneof=chat embeddings reranking"`
	Force       bool                   `json:"force,omitempty"`
	Parameters  schema.ParameterValues `json:"parameters,omitempty"`
	Environment map[string]string      `json:"environment,omitempty"`
}

// Validate validates the load model request
func (req *LoadModelRequest) Validate() error {
	return (pkgConfig.ServiceConfig{}).ValidateRunTarget(req.ModelName, req.Endpoint)
}

// LoadModelResponse represents the response from loading a model
type LoadModelResponse struct {
	BaseURL    string `json:"base_url,omitempty"`
	Reused     bool   `json:"-"`
	InstanceID string `json:"instance_id"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Port       int    `json:"port"`
	Status     string `json:"status"`
	Node       string `json:"node"`
	Message    string `json:"message"`
	// JobID follows the launch on fresh starts and reused runs.
	JobID string `json:"job_id,omitempty"`
}

// routeForModel routes a request based on host or model-aware discovery
func (s *LoadService) routeForModel(ctx context.Context, host, model, path string, body []byte) (*routing.Response, error) {
	if model == "" {
		if host == "" {
			host = "@master"
		}
		return s.router.Unicast(ctx, host, path, "POST", body)
	}
	if host != "" && host != "*" {
		return s.router.Unicast(ctx, host, path, "POST", body)
	}
	return s.cache.RouteToModelOrBroadcast(ctx, model, path, "POST", body)
}

// LoadModel loads a model and returns the instance information
func (s *LoadService) LoadModel(ctx context.Context, req *LoadModelRequest) (*LoadModelResponse, error) {
	if err := validateRunTarget(s.appsConfig, req.Provider, req.ModelName, req.Endpoint); err != nil {
		return nil, err
	}
	slog.Info("[LoadService] LoadModel", "model", req.ModelName, "provider", req.Provider, "node", req.Node, "force", req.Force)

	requestBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := s.routeForModel(ctx, req.Node, req.ModelName, "/zzrouter/v1/internal/runs/load", requestBody)
	if err != nil {
		return nil, newRoutedTransportError(err, req.Node)
	}

	if resp.StatusCode >= http.StatusBadRequest && resp.StatusCode != http.StatusConflict {
		return nil, parseRoutedError(resp.StatusCode, resp.Body, resp.Headers)
	}
	var loadResp LoadModelResponse
	if err := json.Unmarshal(resp.Body, &loadResp); err != nil {
		if resp.StatusCode == http.StatusConflict {
			return nil, parseRoutedError(resp.StatusCode, resp.Body, resp.Headers)
		}
		return nil, newRouteProblemError(http.StatusBadGateway, "Bad Gateway", "upstream returned an invalid load response", httperr.CodeUpstreamShape, resp.Node)
	}

	if resp.StatusCode == http.StatusConflict {
		if loadResp.InstanceID == "" {
			return nil, parseRoutedError(resp.StatusCode, resp.Body, resp.Headers)
		}
		loadResp.Reused = true
	}

	// Use the host from routing response (actual IP we routed to)
	if resp.Node != "" {
		loadResp.Node = resp.Node
	}

	slog.Info("Model loaded successfully", "model", loadResp.Model, "node", loadResp.Node, "port", loadResp.Port)
	return &loadResp, nil
}

// PreviewRunRequest is the same as LoadModelRequest
type PreviewRunRequest = LoadModelRequest

// UnmarshalJSON accepts `model` for model_name, as POST /runs does, and
// keeps refusing unknown fields.
func (req *LoadModelRequest) UnmarshalJSON(data []byte) error {
	type raw LoadModelRequest
	aux := struct {
		ModelAlias string `json:"model"`
		*raw
	}{raw: (*raw)(req)}
	if err := decodeStrict(data, &aux); err != nil {
		return err
	}
	if req.ModelName == "" {
		req.ModelName = aux.ModelAlias
	}
	return nil
}

// PreviewRunResponse represents a run preview
type PreviewRunResponse struct {
	Assessment       *process.MemoryAssessment `json:"assessment,omitempty"`
	Model            string                    `json:"model"`
	Provider         string                    `json:"provider"`
	Node             string                    `json:"node"`
	Port             int                       `json:"port"`
	FullCommand      string                    `json:"full_command"`
	Parameters       map[string]string         `json:"parameters"`
	ParameterSources map[string]string         `json:"parameter_sources"`
	Environment      map[string]string         `json:"environment"`
	WorkingDir       string                    `json:"working_dir"`
}

// PreviewRun returns a preview of what would be run without actually running it
func (s *LoadService) PreviewRun(ctx context.Context, req *PreviewRunRequest) (*PreviewRunResponse, error) {
	if err := validateRunTarget(s.appsConfig, req.Provider, req.ModelName, req.Endpoint); err != nil {
		return nil, err
	}
	slog.Info("[LoadService] PreviewRun", "model", req.ModelName, "provider", req.Provider, "node", req.Node)

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
		return nil, parseRoutedError(resp.StatusCode, resp.Body, resp.Headers)
	}

	var preview PreviewRunResponse
	if err := json.Unmarshal(resp.Body, &preview); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if preview.Assessment == nil {
		preview.Assessment = &process.MemoryAssessment{ContextMode: "unknown", Status: "unknown", UnknownCause: process.NodeOutdated, Reason: process.NodeOutdated.Reason()}
	}

	preview.Assessment.NormalizeLegacy()

	slog.Info("[LoadService] Preview generated for model", "model", preview.Model)
	return &preview, nil
}

func validateRunTarget(configured func() *pkgConfig.AppsConfig, provider, model, endpoint string) error {
	var svc pkgConfig.ServiceConfig
	if configured != nil {
		if cfg := configured(); cfg != nil {
			svc, _ = cfg.LookupApp(provider)
		}
	}
	if err := svc.ValidateRunTarget(model, endpoint); err != nil {
		return newProblemError(http.StatusBadRequest, "Bad Request", err.Error())
	}
	return nil
}
