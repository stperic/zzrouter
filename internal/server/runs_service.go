package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
)

// ErrRunNotFound is returned when a run is not found
var ErrRunNotFound = errors.New("run not found")

// isBroadcastNotFound checks if a response body is an error/problem response
// indicating not-found. This handles the case where broadcast aggregation returns
// a 200 status but the body contains an error from all nodes returning 404.
func isBroadcastNotFound(body []byte) bool {
	var probe struct {
		Status int    `json:"status"`
		Error  string `json:"error"`
	}
	return json.Unmarshal(body, &probe) == nil && (probe.Status == 404 || probe.Error != "")
}

// ============================================================================
// Runs Service - Business Logic Layer
// ============================================================================
//
// Responsibilities:
// - Orchestrate cluster-wide run queries
// - Aggregate results from multiple nodes
// - Apply filtering logic
// - No HTTP concerns (that's the handler's job)
// - No direct provider calls (that's the executor's job)

// RunsService handles runs-related business logic
type RunsService struct {
	router routing.Router
	// Auto-deploy hooks (nil when not wired — caller falls back to the
	// existing sync forward path). Wired at coord only; workers don't
	// have a DeploymentsService, so without this RunsService the
	// public auto_deploy=true path silently no-ops on remote-targeted
	// requests. With it, the chain runs at the coord and forwards the
	// launch to the target node after deploy completes.
	deployments   *DeploymentsService
	modelInPool   func(string) bool
	resolveModel  func(ctx context.Context, name string) (*resolver.Resolved, error)
	appsConfig    func() *pkgConfig.AppsConfig
	jobs          *jobs.Registry
	validateModel func(context.Context, string) error
}

// NewRunsService creates a new runs service
func NewRunsService(router routing.Router) *RunsService {
	return &RunsService{
		router: router,
	}
}

// WithAutoDeploy wires dependencies for the optional deploy-before-launch chain.
func (s *RunsService) WithAutoDeploy(
	deployments *DeploymentsService,
	modelInPool func(string) bool,
	appsConfig func() *pkgConfig.AppsConfig,
	jobsReg *jobs.Registry,
) *RunsService {
	s.deployments = deployments
	s.modelInPool = modelInPool
	s.appsConfig = appsConfig
	s.jobs = jobsReg
	return s
}

// WithModelValidator admits names before warm reuse or auto-deployment.
func (s *RunsService) WithModelValidator(validate func(context.Context, string) error) *RunsService {
	s.validateModel = validate
	return s
}

func (s *RunsService) admitModel(ctx context.Context, model string) error {
	if s.validateModel == nil {
		return nil
	}
	return s.validateModel(ctx, model)
}

// WithModelResolver wires the resolver inference uses, so EnsureRun picks the same node.
func (s *RunsService) WithModelResolver(resolve func(ctx context.Context, name string) (*resolver.Resolved, error)) *RunsService {
	s.resolveModel = resolve
	return s
}

// ListRunsRequest represents a request to list runs
type ListRunsRequest struct {
	Node   string // Node filter (supports wildcards)
	Status string // Retained instance status filter.
	// WithParametersStatus asks each node to report whether its runs still
	// have what their config resolves to. It re-resolves every live run,
	// so it is off unless asked for.
	WithParametersStatus bool
}

// includeParametersStatus is the internal runs listing's ?include value
// asking for each run's parameters_status.
const includeParametersStatus = "parameters_status"

// ListRunsResponse represents the response for list runs
// Uses standard envelope: data, total, has_more
type ListRunsResponse struct {
	Data    []instance.InstanceInfo `json:"data"`
	Total   int                     `json:"total"`
	HasMore bool                    `json:"has_more,omitempty"`
}

// ListRuns retrieves all runs from the cluster
func (s *RunsService) ListRuns(ctx context.Context, req *ListRunsRequest) (*ListRunsResponse, error) {
	slog.Info("[RunsService] ListRuns", "node", req.Node, "status", req.Status)

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	// Use host from request if specified, otherwise broadcast to all (including master)
	targetNode := req.Node
	if targetNode == "" {
		targetNode = "*" // Broadcast to all hosts (workers + master)
	}

	// Route the request (handles local vs cluster logic)
	type runsResponse struct {
		Instances []instance.InstanceInfo `json:"instances"`
	}
	path := "/zzrouter/v1/internal/runs"
	if req.WithParametersStatus {
		path += "?include=" + includeParametersStatus
	}
	runsData, err := routeAndParse[runsResponse](ctx, s.router, "GET", path, targetNode, nil)
	if err != nil {
		return nil, err
	}

	// Apply filters if needed
	filteredRuns := s.applyFilters(runsData.Instances, req)

	slog.Info("[RunsService] Returning runs (filtered from )", "count", len(filteredRuns), "count_2", len(runsData.Instances))

	return &ListRunsResponse{
		Data:    filteredRuns,
		Total:   len(filteredRuns),
		HasMore: false, // Pagination not yet implemented
	}, nil
}

// GetRunRequest represents a request to get a specific run
type GetRunRequest struct {
	RunID string // Run ID
	Node  string // Optional: target specific host
}

// GetRun retrieves a specific run from the cluster
func (s *RunsService) GetRun(ctx context.Context, req *GetRunRequest) (*instance.InstanceInfo, error) {
	slog.Info("[RunsService] GetRun", "id", req.RunID, "node", req.Node)

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	// Create routing request
	routingReq := &routing.Request{
		Path:   fmt.Sprintf("/zzrouter/v1/internal/runs/%s", req.RunID),
		Method: "GET",
		Node:   req.Node,
	}

	// Route the request
	resp, err := s.router.Route(ctx, routingReq)
	if err != nil {
		return nil, newRoutedTransportError(err, req.Node)
	}

	// Check status code
	if resp.StatusCode == http.StatusNotFound {
		slog.Warn("Run not found", "run_id", req.RunID)
		return nil, ErrRunNotFound
	}
	if resp.StatusCode >= 400 {
		slog.Error("[RunsService] Error response for run", "run_id", req.RunID, "status", resp.StatusCode)
		// Preserve upstream status + Problem Details so RespondToError forwards 4xx verbatim instead of 500ing.
		return nil, parseRoutedError(resp.StatusCode, resp.Body, resp.Headers)
	}

	// Parse response
	var run instance.InstanceInfo
	if err := json.Unmarshal(resp.Body, &run); err != nil {
		if isBroadcastNotFound(resp.Body) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if run.ID == "" {
		return nil, ErrRunNotFound
	}

	slog.Info("[RunsService] Found run on host", "run_id", req.RunID, "node", run.Node)
	return &run, nil
}

// MissingRunDetail explains a run id no node lists, from what the cluster
// knows: a stopped run is removed, its log stays on its node, and a live
// run that evicted it names it.
func (s *RunsService) MissingRunDetail(ctx context.Context, id, node string) string {
	runs, err := s.ListRuns(ctx, &ListRunsRequest{Node: node})
	if err == nil {
		for _, run := range runs.Data {
			for _, evicted := range run.EvictedRuns {
				if evicted.RunID == id {
					return fmt.Sprintf("run not found: %s was stopped on node %s to free %d MiB of GPU memory for run %s (%s). A stopped run is removed; its log stays readable at GET /zzrouter/v1/runs/%s/logs?node=%s",
						id, run.Node, evicted.FreedMiB, run.ID, run.Model, id, run.Node)
				}
			}
		}
	}
	return fmt.Sprintf("run not found: %s. A stopped run is removed; its log stays readable at GET /zzrouter/v1/runs/%s/logs?node=<node it ran on>", id, id)
}

// applyFilters applies filtering to the runs list
func (s *RunsService) applyFilters(instances []instance.InstanceInfo, req *ListRunsRequest) []instance.InstanceInfo {
	if req.Status == "" {
		return instances
	}

	filtered := make([]instance.InstanceInfo, 0)
	for _, inst := range instances {
		if string(inst.Status) == req.Status {
			filtered = append(filtered, inst)
		}
	}

	return filtered
}

// StopRunRequest represents a request to stop a run
type StopRunRequest struct {
	RunID string // Run ID
	Node  string // Optional: target specific host
}

// StopRunResponse represents the response from stopping a run
type StopRunResponse struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// StopRun stops a running instance
func (s *RunsService) StopRun(ctx context.Context, req *StopRunRequest) (*StopRunResponse, error) {
	slog.Info("[RunsService] StopRun", "id", req.RunID, "node", req.Node)

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	// Create routing request
	routingReq := &routing.Request{
		Path:   fmt.Sprintf("/zzrouter/v1/internal/runs/%s", req.RunID),
		Method: "DELETE",
		Node:   req.Node,
	}

	// Route the request
	resp, err := s.router.Route(ctx, routingReq)
	if err != nil {
		return nil, newRoutedTransportError(err, req.Node)
	}

	// Check status code
	if resp.StatusCode == http.StatusNotFound {
		slog.Warn("Run not found", "run_id", req.RunID)
		return nil, ErrRunNotFound
	}
	if resp.StatusCode >= 400 {
		slog.Error("[RunsService] Error response for stopping run", "run_id", req.RunID, "status", resp.StatusCode)
		// Preserve upstream status + Problem Details so RespondToError forwards 4xx verbatim instead of 500ing.
		return nil, parseRoutedError(resp.StatusCode, resp.Body, resp.Headers)
	}

	// Parse response
	var stopResp StopRunResponse
	if err := json.Unmarshal(resp.Body, &stopResp); err != nil {
		if isBroadcastNotFound(resp.Body) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// If we got an empty response (all nodes returned not-found), treat as not found
	if stopResp.ID == "" && stopResp.Status == "" {
		return nil, ErrRunNotFound
	}

	slog.Info("Run stopped successfully", "run_id", req.RunID)
	return &stopResp, nil
}

// RestartRunRequest represents a request to restart a run
type RestartRunRequest struct {
	RunID string // Run ID
	Node  string // Optional: target specific host
}

// RestartRunResponse represents the response from restarting a run.
// OldID is the ID of the stopped instance; ID is the new instance's ID
// (restart mints a fresh ID because the port is reallocated).
type RestartRunResponse struct {
	Accepted bool   `json:"accepted"`
	OldID    string `json:"old_id"`
	ID       string `json:"id"`
	JobID    string `json:"job_id,omitempty"` // pkg/jobs handle ID; subscribe via /jobs/:id/stream
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Port     int    `json:"port"`
	Status   string `json:"status"`
	Message  string `json:"message"`
	Node     string `json:"node,omitempty"`
}

// RestartRun restarts an instance
func (s *RunsService) RestartRun(ctx context.Context, req *RestartRunRequest) (*RestartRunResponse, error) {
	slog.Info("[RunsService] RestartRun", "id", req.RunID, "node", req.Node)

	ctx, cancel := ensureTimeout(ctx, constants.ConfiguratorTimeout)
	defer cancel()

	// Route the request
	restartResp, err := routeAndParse[RestartRunResponse](ctx, s.router, "POST", fmt.Sprintf("/zzrouter/v1/internal/runs/%s/restart", req.RunID), req.Node, nil)
	if err != nil {
		return nil, err
	}

	slog.Info("Run restarted successfully", "run_id", req.RunID)
	return restartResp, nil
}

// LaunchRunRequest represents a request to launch a run.
// Field name matches LoadModelRequest/PreviewRunRequest so agents hit one
// consistent shape across /runs, /runs/load, /runs/preview.
type LaunchRunRequest struct {
	Runtime          string `json:"runtime,omitempty"`
	DisposablePlanID string `json:"disposable_plan_id,omitempty"`
	Provider         string `json:"provider" binding:"required"`
	// LaunchMode has one value; omitted means it.
	LaunchMode string `json:"launch_mode" binding:"required,oneof=native"`
	// Accepts `model` (OpenAI-compat, preferred) or `model_name`.
	// UnmarshalJSON below promotes either into this field.
	Model string `json:"model_name" binding:"required"`
	// Endpoint selects the request shape an instance serves. Empty defaults
	// to "chat". Endpoint-aware providers (config declares an `endpoints`
	// overlay) use this to launch a separate instance per shape — e.g.
	// llamacpp needs a dedicated --embeddings process for "embeddings".
	Endpoint string `json:"endpoint,omitempty" binding:"omitempty,oneof=chat embeddings reranking"`
	// AutoDeploy synthesizes a download via the provider's configured
	// registry when the model isn't yet in the cache, then chains the
	// launch. Opt-in: a typo in model_name with auto_deploy:false fails
	// fast with model_not_found instead of triggering a multi-GB pull.
	// Refused for cloud providers (those go through POST /deployments).
	AutoDeploy   bool              `json:"auto_deploy,omitempty"`
	Port         int               `json:"port,omitempty"`
	Files        any               `json:"files,omitempty"` // Will be unmarshaled as []instance.FileMount
	Parameters   map[string]string `json:"parameters,omitempty"`
	EnvVars      map[string]string `json:"env_vars,omitempty"`
	NativeConfig any               `json:"native_config,omitempty"` // Will be unmarshaled as *instance.NativeConfig
	Node         string            `json:"node,omitempty"`          // Target cluster node for routing
}

// UnmarshalJSON accepts `model` as an alias for `model_name`. `model_name`
// wins when both are set; agents should migrate to `model` for parity with
// /v1/* but the older field stays accepted indefinitely.
//
// Decodes strictly. BindJSONStrict sets DisallowUnknownFields on its own
// decoder, but the moment a type implements json.Unmarshaler that setting
// stops applying to it, so this method has to re-assert it or the endpoint
// silently accepts fields it does not implement. That is not theoretical:
// `host` (which the docs used to give as the way to target a worker) was
// dropped on the floor, the model then resolved against the coordinator's
// own store, and the caller got a 500 for a model that was sitting on the
// worker. /runs/preview, which has no custom unmarshaller, rejected the
// same field with a 400 the whole time.
func (r *LaunchRunRequest) UnmarshalJSON(data []byte) error {
	type raw LaunchRunRequest
	aux := struct {
		ModelAlias string `json:"model"`
		*raw
	}{raw: (*raw)(r)}
	if err := decodeStrict(data, &aux); err != nil {
		return err
	}
	if r.Model == "" {
		r.Model = aux.ModelAlias
	}
	if r.LaunchMode == "" {
		r.LaunchMode = string(instance.LaunchModeNative)
	}
	return nil
}

// decodeStrict decodes JSON refusing fields v does not define, the check a
// custom UnmarshalJSON otherwise switches off.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// LaunchRunResponse represents the response from launching a run
type LaunchRunResponse struct {
	Accepted          bool   `json:"accepted"`
	ID                string `json:"id"`
	JobID             string `json:"job_id,omitempty"`        // pkg/jobs handle ID for the launch; subscribe via /jobs/:id/stream
	DeployJobID       string `json:"deploy_job_id,omitempty"` // populated when auto_deploy chains a download; subscribe for pull progress
	Provider          string `json:"provider"`
	LaunchMode        string `json:"launch_mode"`
	Status            string `json:"status"`
	Port              int    `json:"port"`
	HealthURL         string `json:"health_url"`
	StartedAt         string `json:"started_at"`
	Message           string `json:"message"`
	FilesMounted      int    `json:"files_mounted"`
	ParametersApplied int    `json:"parameters_applied"`
	Node              string `json:"node,omitempty"`
}

// LaunchRun launches a new provider instance
// Note: Currently local-only (launches on current host)
// For cluster support, this would route to internal executor
func (s *RunsService) LaunchRun(ctx context.Context, req *LaunchRunRequest) (*LaunchRunResponse, error) {
	// Before auto_deploy can start a download for a name admission refuses.
	if err := s.admitModel(ctx, req.Model); err != nil {
		return nil, err
	}
	slog.Info("[RunsService] LaunchRun", "provider", req.Provider, "launch_mode", req.LaunchMode, "node", req.Node)

	// Launching an instance is unicast by definition — broadcast would
	// fan out to every cluster node and aggregate per-node responses
	// into a wrapper envelope that routeAndParse can't deserialize into
	// LaunchRunResponse. When the caller doesn't specify a node, target
	// the coord-local executor explicitly via @master.
	node := req.Node
	if node == "" {
		node = "@master"
	}

	// Validate parameter keys and values
	if err := validateParameterMaps(req.Parameters, req.EnvVars); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	// Auto-deploy at the public layer when wired and the model is missing
	// from the coord-side pool. This is the only place where auto_deploy
	// works for non-coord targets: workers don't carry a DeploymentsService
	// so the internal handler's auto_deploy gate short-circuits.
	if req.AutoDeploy && s.deployments != nil && s.modelInPool != nil && s.jobs != nil {
		if !s.modelInPool(req.Model) {
			return s.runAutoDeployChain(ctx, req, node)
		}
	}

	ctx, cancel := ensureTimeout(ctx, constants.ConfiguratorTimeout)
	defer cancel()

	// Route to internal executor
	requestBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Route the request (handles local vs cluster logic)
	launchResp, err := routeAndParse[LaunchRunResponse](ctx, s.router, "POST", "/zzrouter/v1/internal/runs", node, requestBody)
	if err != nil {
		return nil, err
	}

	slog.Info("[RunsService] Run launched successfully", "id", launchResp.ID)
	return launchResp, nil
}

// EnsureRunRequest is the body of POST /zzrouter/v1/runs/ensure. Only
// model is required; endpoint defaults to "chat". Other launch knobs
// (provider, parameters, env_vars) are inferred from the cache so the
// agent doesn't need cluster-topology knowledge.
type EnsureRunRequest struct {
	Model    string `json:"model" binding:"required"`
	Endpoint string `json:"endpoint,omitempty" binding:"omitempty,oneof=chat embeddings reranking"`
}

// EnsureRunResponse carries the unified shape for ensure outcomes:
//   - status="running": the run already serves; port, node and run_id are set.
//   - status="starting"|"waiting_for_deploy": subscribe to job_id (and
//     deploy_job_id if non-empty) for readiness.
type EnsureRunResponse struct {
	Status      string `json:"status"`
	Model       string `json:"model"`
	Provider    string `json:"provider,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	Node        string `json:"node,omitempty"`
	Port        int    `json:"port,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	JobID       string `json:"job_id,omitempty"`
	DeployJobID string `json:"deploy_job_id,omitempty"`
	Message     string `json:"message,omitempty"`
}

// EnsureRun makes a model hot on the node the shared resolver picks, so
// ensure and inference agree on where a model runs; that node's run policy
// reuses a live run, and a model not yet on it is deployed first.
func (s *RunsService) EnsureRun(ctx context.Context, req *EnsureRunRequest) (*EnsureRunResponse, error) {
	if err := s.admitModel(ctx, req.Model); err != nil {
		return nil, err
	}
	if s.resolveModel == nil {
		return nil, newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "model resolver not wired")
	}
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = "chat"
	}
	resolved, err := s.resolveModel(ctx, req.Model)
	if err != nil {
		return nil, err
	}
	if resolved.Provider == "" {
		return nil, newProblemError(http.StatusNotFound, "Not Found", fmt.Sprintf("model %q not found in catalog, or not fully downloaded; list /v1/models for known names", req.Model))
	}
	model := resolved.RequestModel
	if model == "" {
		model = resolved.ModelName
	}
	launchResp, err := s.LaunchRun(ctx, &LaunchRunRequest{
		Provider:   resolved.Provider,
		LaunchMode: string(instance.LaunchModeNative),
		Model:      model,
		Endpoint:   endpoint,
		AutoDeploy: true,
		Node:       resolved.Node,
	})
	if err != nil {
		return nil, err
	}
	return &EnsureRunResponse{
		Status:      launchResp.Status,
		Model:       model,
		Provider:    launchResp.Provider,
		Endpoint:    endpoint,
		Node:        launchResp.Node,
		Port:        launchResp.Port,
		RunID:       launchResp.ID,
		JobID:       launchResp.JobID,
		DeployJobID: launchResp.DeployJobID,
		Message:     launchResp.Message,
	}, nil
}

// runAutoDeployChain synthesizes a deploy + chains the launch behind it.
// Returns 202 immediately with deploy_job_id + run_job_id; a goroutine
// subscribes to the deploy job and forwards the launch (with auto_deploy=false)
// to `node` once the download completes. Mirrors the executor-level chain
// but lives here so non-coord targets get the same semantics.
func (s *RunsService) runAutoDeployChain(ctx context.Context, req *LaunchRunRequest, node string) (*LaunchRunResponse, error) {
	cfg := s.appsConfig()
	if cfg == nil {
		return nil, newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "apps config unavailable")
	}
	svc, ok := cfg.LookupApp(req.Provider)
	if !ok {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request", fmt.Sprintf("provider %q not in config", req.Provider))
	}
	if prov_apps.Lifecycle(svc) == prov_apps.LifecycleNone {
		forward := *req
		forward.AutoDeploy = false
		return s.LaunchRun(ctx, &forward)
	}
	deployReq, err := autoDeployRequest(req.Provider, svc, req.Model)
	if err != nil {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request", err.Error())
	}
	if node != "" && node != "@master" {
		deployReq.Nodes = []string{node}
	}

	// Detached ctx: deploy lifetime outlives the originating request. WithoutCancel
	// rather than Background so the deploy keeps the request's trace and values —
	// Background would orphan its spans from the request that caused them.
	deployment, err := s.deployments.Deploy(context.WithoutCancel(ctx), deployReq)
	if err != nil {
		return nil, newProblemError(http.StatusInternalServerError, "Internal Server Error", "deploy failed: "+err.Error())
	}

	//nolint:contextcheck // detached on purpose: the job carries its own context and outlives this request
	runHandle, err := s.jobs.StartDetached(jobs.KindRun, "", jobs.Meta{
		"model":         req.Model,
		"provider":      req.Provider,
		"endpoint":      req.Endpoint,
		"target_node":   node,
		"deployment_id": deployment.ID,
		"phase":         "waiting_for_deploy",
		"chained_via":   "auto_deploy_xnode",
	})
	if err != nil {
		return nil, newProblemError(http.StatusInternalServerError, "Internal Server Error", "open run job: "+err.Error())
	}

	// Forward request copy: clear AutoDeploy so the internal hop on the
	// target doesn't recurse, and append the resolved file as a #hint so
	// the launcher's resolver picks the right variant inside the repo dir.
	forwardReq := *req
	forwardReq.AutoDeploy = false
	forwardReq.Node = node
	if deployReq.File != "" && !strings.Contains(forwardReq.Model, "#") {
		forwardReq.Model = forwardReq.Model + "#" + deployReq.File
	}

	//nolint:contextcheck // the goroutine runs on runHandle.Context(), which is the right lifetime here
	go s.waitDeployThenForward(runHandle, deployment.ID, forwardReq, node)

	return &LaunchRunResponse{
		ID:          "",
		JobID:       runHandle.ID(),
		DeployJobID: deploymentLocalJobID(deployment, node),
		Provider:    req.Provider,
		LaunchMode:  req.LaunchMode,
		Status:      "waiting_for_deploy",
		Node:        node,
		Message:     "deploy chain accepted; subscribe to deploy_job_id for download progress, then job_id for launch readiness",
	}, nil
}

// waitDeployThenForward waits for the target's deployment job and launch
// readiness. The chain context owns both waits.
func (s *RunsService) waitDeployThenForward(runHandle jobs.Handle, deploymentID string, req LaunchRunRequest, node string) {
	const chainTimeout = 60 * time.Minute
	ctx, cancel := context.WithTimeout(runHandle.Context(), chainTimeout)
	defer cancel()
	if err := s.deployments.tracker.WaitNode(ctx, deploymentID, node); err != nil {
		runHandle.Fail(err)
		return
	}
	runHandle.Meta(jobs.Meta{"phase": "launching"})
	body, err := json.Marshal(req)
	if err != nil {
		runHandle.Fail(err)
		return
	}
	resp, err := routeAndParse[LaunchRunResponse](ctx, s.router, "POST", "/zzrouter/v1/internal/runs", node, body)
	if err == nil && resp.JobID != "" {
		err = s.deployments.jobs.Wait(ctx, resp.JobID, node)
	}
	if err != nil {
		runHandle.Fail(fmt.Errorf("launch after deploy on %s: %w", node, err))
		return
	}
	runHandle.Meta(jobs.Meta{"phase": "running", "instance_id": resp.ID, "port": resp.Port})
	runHandle.Done()
}

// GetRunHealthRequest represents a request to get run health
type GetRunHealthRequest struct {
	RunID string // Run ID
	Node  string // Optional: target specific host
}

// GetRunHealthResponse represents the response for run health
type GetRunHealthResponse struct {
	InstanceID string `json:"instance_id"`
	Status     string `json:"status"`
	HealthURL  string `json:"health_url"`
	Uptime     string `json:"uptime"`
	LastCheck  string `json:"last_check,omitempty"`
}

// GetRunHealth gets the health status of a run
func (s *RunsService) GetRunHealth(ctx context.Context, req *GetRunHealthRequest) (*GetRunHealthResponse, error) {
	slog.Info("[RunsService] GetRunHealth", "id", req.RunID, "node", req.Node)

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	// Route the request
	healthResp, err := routeAndParse[GetRunHealthResponse](ctx, s.router, "GET", fmt.Sprintf("/zzrouter/v1/internal/runs/%s/health", req.RunID), req.Node, nil)
	if err != nil {
		return nil, err
	}

	slog.Info("[RunsService] Run health retrieved", "id", req.RunID, "status", healthResp.Status)
	return healthResp, nil
}
