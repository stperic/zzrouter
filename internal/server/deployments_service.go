package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"time"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ============================================================================
// Deployments Service - Business Logic Layer
// ============================================================================
//
// A Deployment represents "place this model on these nodes" — the unit of work
// that replaces the former one-off and multi-node /pulls split. A deploy may
// download (for local repos) or register (for cloud providers); both shapes
// produce the same job-shaped response. Per-node execution fans out to the
// internal /zzrouter/v1/internal/deployments primitive.

// DeploymentsService orchestrates a deployment across one or many target nodes.
type DeploymentsService struct {
	router     routing.Router
	tracker    *DeploymentTracker
	nodeInfo   NodeNamer
	appsConfig func() *pkgConfig.AppsConfig
	repoFiles  func(context.Context, string) ([]metadata.TreeFileEntry, error)
	refresher  *runsRefresher

	// resolveNodeURL returns a target node's public URL, used as the
	// SourceURL when dispatching /sync/deploy. Optional — when nil,
	// peer sync is skipped and Deploy falls back to parallel pulls
	// from the registry on every target.
	resolveNodeURL func(node string) string
}

// NewDeploymentsService creates a DeploymentsService.
func NewDeploymentsService(router routing.Router, nodeInfo NodeNamer, appsConfig func() *pkgConfig.AppsConfig) *DeploymentsService {
	return &DeploymentsService{
		router:     router,
		tracker:    NewDeploymentTracker(),
		nodeInfo:   nodeInfo,
		appsConfig: appsConfig,
	}
}

// WithNodeURLResolver wires a node-name → public-URL resolver. When
// present, multi-node deploys prefer peer sync over parallel registry
// pulls: the first node that already has the model becomes the source
// and /sync/deploy dispatches to the remaining targets. Safe to skip
// for single-node callers and in tests.
func (s *DeploymentsService) WithNodeURLResolver(fn func(node string) string) *DeploymentsService {
	s.resolveNodeURL = fn
	return s
}

// isCloudRegistry returns true if the given repo corresponds to a cloud provider.
func (s *DeploymentsService) isCloudRegistry(repoName string) bool {
	cfg := s.appsConfig()
	if cfg == nil || repoName == "" {
		return false
	}
	appCfg, exists := cfg.LookupApp(repoName)
	return exists && appCfg.IsCloudProvider()
}

// GetTracker returns the underlying DeploymentTracker for callers that need it
// directly (e.g., sync callbacks updating per-node byte progress).
func (s *DeploymentsService) GetTracker() *DeploymentTracker {
	return s.tracker
}

// DeployRequest is the public request shape — a single endpoint handles zero,
// one, or many target nodes.
type DeployRequest struct {
	Provider        string    `json:"provider,omitempty"`
	Features        *[]string `json:"features,omitempty"`
	Restart         string    `json:"restart,omitempty"`
	defaultFeatures bool
	Model           string   `json:"model" binding:"required"`
	Nodes           []string `json:"nodes,omitempty"`
	Registry        string   `json:"registry,omitempty"`
	File            string   `json:"file,omitempty"`
	Format          string   `json:"format,omitempty"`
	Force           bool     `json:"force,omitempty"`
}

// Validate performs structural validation. Resolution of Nodes (cloud/shared
// collapsing, empty-list handling) happens in Deploy.
func (req *DeployRequest) Validate() error {
	if req.Model == "" {
		return fmt.Errorf("model is required")
	}
	if req.Restart != "" && req.Restart != restartAffected {
		return fmt.Errorf("restart must be %q", restartAffected)
	}
	return nil
}

// internalDeploymentResult matches the wire shape returned by the internal
// /zzrouter/v1/internal/deployments endpoint (see deployments_executor.go).
type internalDeploymentResult struct {
	Key      string `json:"key"`
	Model    string `json:"model"`
	Registry string `json:"registry"`
	Node     string `json:"node"`
	Status   string `json:"status"`
	Message  string `json:"message"`
	// JobID, when non-empty, is the pkg/jobs handle tracking this
	// download. Clients hit /zzrouter/v1/jobs/:id/stream to watch
	// progress. Empty when jobs integration was skipped (e.g. a
	// cached-dedupe early return that doesn't spawn a new producer).
	JobID string `json:"job_id,omitempty"`
}

// Deploy fans the request out to the resolved set of target nodes, creating a
// tracked Deployment and synthesizing per-node internal calls.
func (s *DeploymentsService) Deploy(ctx context.Context, req *DeployRequest) (*Deployment, error) {
	if err := req.Validate(); err != nil {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request", err.Error())
	}

	ctx, cancel := ensureTimeout(ctx, constants.ClusterDefaultTimeout)
	defer cancel()

	// Parse req.Model for '@node' and '#file' suffixes so direct API consumers
	// (curl, non-CLI clients) get the same extraction the CLI does client-side.
	// Explicit fields on the request always win over parsed suffixes.
	if id, perr := modelregistry.Parse(req.Model, modelregistry.ParseOpts{
		Config:   s.appsConfig(),
		Registry: req.Registry,
	}); perr == nil && id != nil {
		if clean := id.GetFullModelName(); clean != "" {
			req.Model = clean
		}
		if req.File == "" && id.File != "" {
			req.File = id.File
		}
		if len(req.Nodes) == 0 && id.Node != "" {
			req.Nodes = []string{id.Node}
		}
	}

	nodes, err := s.resolveNodes(req)
	if err != nil {
		return nil, err
	}
	plans, err := s.planDeploy(ctx, req, nodes)
	if err != nil {
		return nil, planError(err)
	}
	if req.Restart != "" && (s.refresher == nil || s.refresher.jobs == nil) {
		return nil, newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "deployment restarts are unavailable")
	}

	d := s.tracker.CreateDeployment(req.Model, req.Format, req.Registry, req.File, nodes)

	// Partition targets by whether they already have the model. A node
	// that has it becomes the sync source for ones that don't, so the
	// cluster transfers 1× from the registry + N× peer sync instead of
	// N× registry pulls. When no node has it (or lookup is wired off),
	// the partition collapses to all-needs and we fall through to the
	// historical parallel registry-pull fan-out.
	var haves, needs []string
	if plans == nil && !req.Force {
		haves, needs = s.partitionByModelPresence(ctx, nodes, req.Model, req.Format, nil)
	} else {
		needs = nodes
	}

	source := ""
	if len(haves) > 0 && s.resolveNodeURL != nil {
		source = haves[0]
	}

	// haves are no-ops for the deployment proper — mark them completed
	// so the tracker reflects reality and the Active Deploys view
	// doesn't show pointless rows for already-synced nodes.
	for _, node := range haves {
		_ = s.tracker.UpdateNodeStatus(d.ID, node, constants.StatusCompleted, "already present")
	}

	for _, node := range needs {
		if err := s.dispatchDeployTarget(ctx, d.ID, node, source, req, plans[node], nodes); err != nil {
			return nil, err
		}
	}

	status, message := aggregateStatus(s.tracker.GetDeployment(d.ID))
	_ = s.tracker.UpdateDeploymentStatus(d.ID, status, message)
	if req.Restart == restartAffected {
		//nolint:contextcheck // the chain outlives this request and runs on its job context
		jobID, err := s.restartAfterDeploy(s.tracker.GetDeployment(d.ID), plans)
		if err != nil {
			return nil, err
		}
		_ = s.tracker.SetRestartJobID(d.ID, jobID)
	}

	// Return the up-to-date deployment snapshot (includes per-node download IDs).
	return s.tracker.GetDeployment(d.ID), nil
}

func (s *DeploymentsService) dispatchDeployTarget(ctx context.Context, deploymentID, node, source string, req *DeployRequest, plan deployPlan, nodes []string) error {
	model := req.Model
	if plan.Download != nil {
		model = plan.Download.Repo
	}
	nodeSource := source
	if plan.Download != nil && !req.Force && s.resolveNodeURL != nil {
		candidates, _ := s.partitionByModelPresence(ctx, nodes, model, req.Format, plan.Download)
		for _, candidate := range candidates {
			if candidate == node {
				nodeSource = ""
				break
			}
			if nodeSource == "" {
				nodeSource = candidate
			}
		}
	}
	// Peer sync path: source has the model and we can derive its
	// public URL — dispatch /sync/deploy so the worker pulls from
	// within the cluster. Falls back to the registry-pull path if
	// either condition is missing.
	if nodeSource != "" && node != nodeSource && !req.Force {
		if ok := s.dispatchSyncDeploy(ctx, deploymentID, node, nodeSource, req, plan); ok {
			return nil
		}
	}

	body, err := json.Marshal(map[string]any{
		"model":    model,
		"node":     node,
		"registry": req.Registry,
		"file":     req.File,
		"force":    req.Force,
		"provider": plan.Provider,
		"features": plan.Features,
		"download": plan.Download,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	result, err := routeAndParse[internalDeploymentResult](ctx, s.router, "POST", "/zzrouter/v1/internal/deployments", node, body)
	if err != nil {
		slog.Error("[DeploymentsService] Failed to start deployment", "node", node, "error", err)
		_ = s.tracker.UpdateNodeStatus(deploymentID, node, constants.StatusFailed, err.Error())
		return nil
	}

	if result.Key == "" {
		msg := fmt.Sprintf("backend returned empty pull response for model %q", req.Model)
		_ = s.tracker.UpdateNodeStatus(deploymentID, node, constants.StatusFailed, msg)
		return nil
	}

	// A node answers terminally when there was nothing to fetch: a
	// cloud model is registered with its provider on the spot. Writing
	// `downloading` over that answer strands the deployment, because no
	// producer will ever report progress against it.
	//
	// mapLiveStatus is the allowlist an unknown wire value must clear;
	// cancelled is excluded because constants documents it as
	// deployment-level only, so a node may not claim it.
	if reported, ok := mapLiveStatus(result.Status); ok &&
		reported.IsTerminal() && reported != constants.StatusCancelled {
		errMsg := ""
		if reported == constants.StatusFailed {
			errMsg = result.Message
		}
		_ = s.tracker.UpdateNodeStatus(deploymentID, node, reported, errMsg)
		_ = s.tracker.UpdateNodeDownloadID(deploymentID, node, result.Key)
		if result.JobID != "" {
			_ = s.tracker.UpdateNodeJobID(deploymentID, node, result.JobID)
		}
		return nil
	}

	_ = s.tracker.UpdateNodeStatus(deploymentID, node, constants.StatusDownloading, "")
	_ = s.tracker.UpdateNodeDownloadID(deploymentID, node, result.Key)
	if result.JobID != "" {
		_ = s.tracker.UpdateNodeJobID(deploymentID, node, result.JobID)
	}
	return nil
}

// resolveNodes applies cloud collapsing rules and dedups the requested
// node list. Returns a 400 problem when no target can be inferred.
func (s *DeploymentsService) resolveNodes(req *DeployRequest) ([]string, error) {
	// Cloud providers and Ollama cloud variants always route to the coordinator.
	if s.nodeInfo != nil && (s.isCloudRegistry(req.Registry) || isOllamaCloudVariant(req.Model, req.File)) {
		return []string{s.nodeInfo.GetNodename()}, nil
	}

	if len(req.Nodes) == 0 {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request",
			"nodes is required: client must select compatible node(s) before downloading")
	}

	seen := make(map[string]bool, len(req.Nodes))
	deduped := make([]string, 0, len(req.Nodes))
	for _, n := range req.Nodes {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		deduped = append(deduped, n)
	}

	if len(deduped) == 0 {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request",
			"nodes is required: client must select compatible node(s) before downloading")
	}

	return deduped, nil
}

// StopDownloadRequest represents a request to stop a single per-node download.
type StopDownloadRequest struct {
	DownloadID string
	Node       string
}

// StopDownloadResponse represents the response from stopping a download.
type StopDownloadResponse struct {
	Key     string `json:"key"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// StopDownload stops a single per-node download by ID. Used internally by the
// cancel flow; remote *RoutedError values are forwarded verbatim so status
// codes survive, and transport failures fall back to a local attempt.
func (s *DeploymentsService) StopDownload(ctx context.Context, req *StopDownloadRequest) (*StopDownloadResponse, error) {
	node := req.Node
	if node == "" {
		if parts := strings.SplitN(req.DownloadID, "/", 2); len(parts) >= 2 {
			node = parts[0]
		}
	}

	slog.Info("[DeploymentsService] StopDownload", "id", req.DownloadID, "node", node)

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	stopPath := "/zzrouter/v1/internal/deployments/stop?key=" + url.QueryEscape(req.DownloadID)

	stopResp, err := routeAndParse[StopDownloadResponse](ctx, s.router, "DELETE", stopPath, node, nil)
	if err == nil {
		slog.Info("Download stopped successfully", "download_id", req.DownloadID)
		return stopResp, nil
	}

	// Remote returned a well-formed Problem Details — surface directly.
	var remoteRouted *RoutedError
	if errors.As(err, &remoteRouted) && remoteRouted.ProblemDetails != nil {
		return nil, err
	}

	slog.Warn("[DeploymentsService] Remote stop failed, trying local",
		"id", req.DownloadID, "node", node, "error", err)

	localResp, localErr := routeAndParse[StopDownloadResponse](ctx, s.router, "DELETE", stopPath, "", nil)
	if localErr == nil {
		return localResp, nil
	}

	return nil, errors.Join(err, localErr)
}

// StopAllDownloadsResponse is the response from stopping all downloads on a node.
type StopAllDownloadsResponse struct {
	Stopped int      `json:"stopped"`
	Message string   `json:"message"`
	Errors  []string `json:"errors,omitempty"`
}

// GetDeployment returns a tracked deployment by ID with per-node progress
// merged from the authoritative DownloadManagers on each target node.
func (s *DeploymentsService) GetDeployment(ctx context.Context, id string) *Deployment {
	d := s.tracker.GetDeployment(id)
	if d == nil {
		return nil
	}
	s.mergeProgress(ctx, d)
	return d
}

// ListDeployments returns every tracked deployment (optionally filtered to
// active) with per-node progress merged in.
func (s *DeploymentsService) ListDeployments(ctx context.Context, activeOnly bool) []*Deployment {
	deployments := s.tracker.ListDeployments(activeOnly)
	for _, d := range deployments {
		s.mergeProgress(ctx, d)
	}
	return deployments
}

// mergeProgress overlays live per-node download state from each target node's
// internal /deployments endpoint onto the tracker's deployment snapshot.
//
// Design: compose on read. The tracker is authoritative for creation-time
// fields (model, registry, node list, download IDs, cancellation intent). The
// per-node DownloadManager is authoritative for live bytes/progress/speed.
// Failures to reach a single node are logged and skipped — a missing node
// must not blow up the whole response.
//
// mergeProgress mutates d in place and does NOT write back to the tracker.
func (s *DeploymentsService) mergeProgress(ctx context.Context, d *Deployment) {
	if d == nil || len(d.Nodes) == 0 {
		return
	}

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	// Group nodes with live downloads to fan out one call per target node.
	byNode := make(map[string][]int, len(d.Nodes))
	for i, n := range d.Nodes {
		if n.DownloadID == "" {
			continue
		}
		byNode[n.Node] = append(byNode[n.Node], i)
	}

	for node, idxs := range byNode {
		envelope, err := routeAndParse[listDeploymentsEnvelope](
			ctx, s.router, "GET", "/zzrouter/v1/internal/deployments", node, nil,
		)
		if err != nil {
			slog.Debug("[DeploymentsService] mergeProgress: failed to fetch live downloads",
				"node", node, "error", err)
			continue
		}

		byKey := make(map[string]DownloadSummary, len(envelope.Data))
		for _, s := range envelope.Data {
			byKey[s.Key] = s
		}

		for _, i := range idxs {
			dn := &d.Nodes[i]
			live, ok := byKey[dn.DownloadID]
			if !ok {
				continue
			}
			dn.BytesDownloaded = live.BytesDownloaded
			dn.BytesTotal = live.BytesTotal
			dn.Progress = live.Progress
			dn.Speed = live.Speed
			if live.StartedAt != "" {
				if t, err := time.Parse(time.RFC3339, live.StartedAt); err == nil {
					dn.StartedAt = t
				}
			}
			if mapped, ok := mapLiveStatus(live.Status); ok {
				dn.Status = mapped
			}
			if constants.Status(live.Status).IsTerminal() {
				if live.Error != "" {
					dn.Error = live.Error
				}
				if dn.CompletedAt.IsZero() {
					dn.CompletedAt = utils.Now()
				}
			}
		}
	}

	// Recompute summary counters and aggregate status from the merged view.
	d.UpdateSummary()
	d.Status, d.Message = aggregateStatus(d)
}

// mapLiveStatus casts a live download status string to constants.Status.
// Now that both sides share the same vocabulary, this is a simple cast with
// an unknown-value guard. The second return is false for unrecognized values.
func mapLiveStatus(live string) (constants.Status, bool) {
	s := constants.Status(live)
	switch s {
	case constants.StatusPending,
		constants.StatusDownloading,
		constants.StatusCompleted,
		constants.StatusFailed,
		constants.StatusCancelled,
		constants.StatusSkipped:
		return s, true
	}
	return "", false
}

// aggregateStatus returns the deployment-level status+message implied by its
// per-node slice. It is the only aggregator: Deploy calls it once the fan-out
// settles and mergeProgress calls it after every live merge, so the status in
// a POST response and the status on the next GET cannot disagree.
//
// Counting walks the node slice rather than the summary counters because
// UpdateSummary has no bucket for every status a node can hold — a peer-sync
// node sits in "syncing", which is neither complete nor failed, and reading
// the counters alone reports it as a failure the moment it is dispatched.
func aggregateStatus(d *Deployment) (constants.Status, string) {
	if len(d.Nodes) == 0 {
		return d.Status, d.Message
	}

	// Preserve terminal cancellation — a user cancel shouldn't be overwritten
	// by the merge just because a late live update arrived.
	if d.Status == constants.StatusCancelled {
		return d.Status, d.Message
	}

	total := len(d.Nodes)
	var inFlight, failed, skipped int
	for _, n := range d.Nodes {
		switch {
		case !n.Status.IsTerminal():
			inFlight++
		case n.Status == constants.StatusFailed:
			failed++
		case n.Status == constants.StatusSkipped:
			skipped++
		}
	}

	switch {
	case inFlight > 0:
		return constants.StatusDownloading, fmt.Sprintf("Downloading on %d/%d nodes", inFlight, total)
	case failed == total:
		return constants.StatusFailed, "All nodes failed"
	case failed > 0:
		return constants.StatusFailed, fmt.Sprintf("%d/%d nodes failed", failed, total)
	case skipped == total:
		// Terminal and nothing went wrong, but nothing landed either; saying
		// "complete" here would claim a deployment that never happened.
		return constants.StatusCompleted, fmt.Sprintf("All %d nodes skipped", total)
	default:
		return constants.StatusCompleted, fmt.Sprintf("All %d nodes complete", total)
	}
}

// CancelDeployment cancels a tracked deployment and stops every still-running
// per-node download associated with it. Per-node stop failures are logged but
// do not block the cancel — a missing deployment returns an error so the
// controller can 404.
func (s *DeploymentsService) CancelDeployment(ctx context.Context, id string) error {
	d := s.tracker.GetDeployment(id)
	if d == nil {
		return fmt.Errorf("deployment not found: %s", id)
	}
	if d.RestartJobID != "" && s.refresher != nil && s.refresher.jobs != nil {
		_ = s.refresher.jobs.Cancel(d.RestartJobID)
	}

	for _, n := range d.Nodes {
		if n.JobID != "" {
			_, err := s.router.Route(ctx, &routing.Request{Method: "POST", Node: n.Node, Path: "/zzrouter/v1/internal/jobs/" + url.PathEscape(n.JobID) + "/cancel"})
			if err != nil {
				slog.Warn("failed to cancel placement job", "job", n.JobID, "error", err)
			}
		}
		if n.DownloadID == "" {
			continue
		}
		if _, err := s.StopDownload(ctx, &StopDownloadRequest{
			DownloadID: n.DownloadID,
			Node:       n.Node,
		}); err != nil {
			slog.Warn("[DeploymentsService] Failed to stop per-node download during cancel",
				"deployment_id", id, "node", n.Node, "download_id", n.DownloadID, "error", err)
		}
	}

	return s.tracker.CancelDeployment(id)
}

// CancelDeploymentNode cancels a single node within a deployment. Returns a
// problem error the controller can forward with the correct status when the
// deployment or node is missing, or when the node is already terminal.
func (s *DeploymentsService) CancelDeploymentNode(ctx context.Context, id, node string) error {
	d := s.tracker.GetDeployment(id)
	if d == nil {
		return newProblemError(http.StatusNotFound, "Not Found",
			fmt.Sprintf("deployment not found: %s", id))
	}

	var target *DeploymentNode
	for i := range d.Nodes {
		if d.Nodes[i].Node == node {
			target = &d.Nodes[i]
			break
		}
	}
	if target == nil {
		return newProblemError(http.StatusNotFound, "Not Found",
			fmt.Sprintf("node not found in deployment: %s", node))
	}

	switch target.Status {
	case constants.StatusCompleted, constants.StatusFailed, constants.StatusSkipped:
		return newProblemError(http.StatusBadRequest, "Bad Request",
			fmt.Sprintf("node %s is already in terminal state %q", node, target.Status))
	}

	if target.DownloadID != "" {
		if _, err := s.StopDownload(ctx, &StopDownloadRequest{
			DownloadID: target.DownloadID,
			Node:       node,
		}); err != nil {
			slog.Warn("[DeploymentsService] Per-node cancel: remote stop failed; marking failed anyway",
				"deployment_id", id, "node", node, "download_id", target.DownloadID, "error", err)
		}
	}
	if target.JobID != "" {
		_, err := s.router.Route(ctx, &routing.Request{Method: "POST", Node: node, Path: "/zzrouter/v1/internal/jobs/" + url.PathEscape(target.JobID) + "/cancel"})
		if err != nil {
			slog.Warn("failed to cancel placement job", "job", target.JobID, "error", err)
		}
	}

	if err := s.tracker.UpdateNodeStatus(id, node, constants.StatusFailed, "cancelled by user"); err != nil {
		return err
	}

	// Recompute aggregate status so the deployment reflects the change.
	refreshed := s.tracker.GetDeployment(id)
	if refreshed != nil {
		status, message := aggregateStatus(refreshed)
		_ = s.tracker.UpdateDeploymentStatus(id, status, message)
	}
	return nil
}

// CancelNodeAcrossDeployments cancels every non-terminal slice that targets
// `node` across all active deployments. Returns the count cancelled.
func (s *DeploymentsService) CancelNodeAcrossDeployments(ctx context.Context, node string) int {
	if node == "" {
		return 0
	}
	cancelled := 0
	for _, d := range s.tracker.ListDeployments(true) {
		for _, n := range d.Nodes {
			if n.Node != node {
				continue
			}
			switch n.Status {
			case constants.StatusCompleted, constants.StatusFailed, constants.StatusSkipped:
				continue
			}
			if err := s.CancelDeploymentNode(ctx, d.ID, node); err != nil {
				slog.Warn("[DeploymentsService] host-scoped cancel: node cancel failed",
					"deployment_id", d.ID, "node", node, "error", err)
				continue
			}
			cancelled++
		}
	}
	return cancelled
}

// CancelAllDeployments cancels every active deployment. Returns the count.
func (s *DeploymentsService) CancelAllDeployments(ctx context.Context) int {
	active := s.tracker.ListDeployments(true)
	for _, d := range active {
		if err := s.CancelDeployment(ctx, d.ID); err != nil {
			slog.Warn("[DeploymentsService] Failed to cancel deployment", "deployment_id", d.ID, "error", err)
		}
	}
	return len(active)
}

// planError answers a planning failure by whose it is: the caller's
// request (400), a server-side failure here or on a node (its own 5xx), or a
// node or registry that did not answer usefully (502). A node's 4xx to an
// internal call is a fault between nodes, never the caller's, so it is a
// 502 too.
func planError(err error) error {
	var routed *RoutedError
	switch {
	case isInvalidInput(err):
		return newProblemError(http.StatusBadRequest, "Bad Request", err.Error())
	case errors.As(err, &routed) && routed.StatusCode >= http.StatusInternalServerError:
		return err
	default:
		return newProblemError(http.StatusBadGateway, "Bad Gateway", err.Error())
	}
}
