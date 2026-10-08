package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/routing"
)

// Peer-sync fan-out helpers for DeploymentsService. These sit behind
// the main Deploy() path so the existing registry-pull flow stays
// readable; peer sync is opt-in (WithNodeURLResolver) and falls back
// cleanly when any prerequisite is missing.

// syncExistsResponse matches the /internal/sync/exists handler.
type syncExistsResponse struct {
	Exists bool   `json:"exists"`
	Model  string `json:"model"`
	Format string `json:"format"`
}

// syncDeployAsyncResponse matches /internal/sync/deploy's 202 payload.
// LocalJobID is the subscribable handle ID on the worker; JobID echoes
// the caller-supplied trace ID (empty when we don't pass one).
type syncDeployAsyncResponse struct {
	Status     string `json:"status"`
	Model      string `json:"model"`
	Format     string `json:"format"`
	Source     string `json:"source"`
	JobID      string `json:"job_id"`
	LocalJobID string `json:"local_job_id"`
	Message    string `json:"message"`
}

// partitionByModelPresence probes each node's /internal/sync/exists
// endpoint and splits the input into (haves, needs). Probe errors or
// a missing format default to "doesn't have" — safer to fan out a
// registry pull than to skip a legitimate target. Probes run serially
// because N is small (<= a handful) and parallelism isn't worth the
// goroutine bookkeeping here.
func (s *DeploymentsService) partitionByModelPresence(ctx context.Context, nodes []string, model, format string, download *metadata.DownloadRequest) (haves, needs []string) {
	if format == "" {
		format = "gguf"
	}
	q := url.Values{}
	q.Set("model", model)
	q.Set("format", format)
	path := "/zzrouter/v1/internal/sync/exists?" + q.Encode()
	method := "GET"
	var body []byte
	if download != nil {
		method = "POST"
		body, _ = json.Marshal(download)
	}

	for _, node := range nodes {
		resp, err := s.router.Route(ctx, &routing.Request{
			Path:   path,
			Method: method,
			Body:   body,
			Node:   node,
		})
		if err != nil || resp == nil || resp.StatusCode >= 400 {
			needs = append(needs, node)
			continue
		}
		var parsed syncExistsResponse
		if jerr := json.Unmarshal(resp.Body, &parsed); jerr != nil {
			needs = append(needs, node)
			continue
		}
		if parsed.Exists {
			haves = append(haves, node)
		} else {
			needs = append(needs, node)
		}
	}
	return haves, needs
}

// dispatchSyncDeploy POSTs /internal/sync/deploy on target and, on
// success, populates the tracker's per-node JobID with the worker's
// local_job_id so Active Deploys subscribers see the sync's KindSync
// stream. Returns true only when the dispatch was accepted and a
// subscribable job_id came back. Callers fall through to the normal
// registry-pull path on false.
func (s *DeploymentsService) dispatchSyncDeploy(ctx context.Context, deploymentID, target, source string, req *DeployRequest, plan deployPlan) bool {
	if s.resolveNodeURL == nil {
		return false
	}
	sourceURL := s.resolveNodeURL(source)
	if sourceURL == "" {
		slog.Warn("[DeploymentsService] peer-sync skipped: no public URL for source",
			"source", source, "target", target)
		return false
	}

	model := req.Model
	if plan.Download != nil {
		model = plan.Download.Repo
	}
	body, err := json.Marshal(map[string]any{
		"model":       model,
		"format":      formatOrDefault(req.Format),
		"source_node": source,
		"source_url":  sourceURL,
		"job_id":      deploymentID,
		"download":    plan.Download,
		"provider":    plan.Provider,
		"features":    plan.Features,
	})
	if err != nil {
		slog.Warn("[DeploymentsService] peer-sync skipped: marshal failed",
			"target", target, "error", err)
		return false
	}

	resp, err := s.router.Route(ctx, &routing.Request{
		Path:   "/zzrouter/v1/internal/sync/deploy",
		Method: "POST",
		Node:   target,
		Body:   body,
	})
	if err != nil {
		slog.Warn("[DeploymentsService] peer-sync dispatch failed: falling back to registry pull",
			"target", target, "error", err)
		return false
	}
	if resp == nil || resp.StatusCode >= 400 {
		slog.Warn("[DeploymentsService] peer-sync rejected: falling back to registry pull",
			"target", target, "status", statusOf(resp))
		return false
	}

	var parsed syncDeployAsyncResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		slog.Warn("[DeploymentsService] peer-sync: cannot decode 202 envelope",
			"target", target, "error", err)
		return false
	}

	status := constants.Status(syncStatusLabel)
	msg := fmt.Sprintf("syncing from %s", source)
	_ = s.tracker.UpdateNodeStatus(deploymentID, target, status, msg)
	_ = s.tracker.UpdateNodeSource(deploymentID, target, source)
	if parsed.LocalJobID != "" {
		_ = s.tracker.UpdateNodeJobID(deploymentID, target, parsed.LocalJobID)
	}
	return true
}

// syncStatusLabel is the per-node tracker status that signals "pulling
// from a peer via /sync/deploy" rather than the normal "downloading
// from registry." Stays a string literal on the deployment side; the
// TUI renders both as live/in-flight.
const syncStatusLabel = "syncing"

func formatOrDefault(f string) string {
	if f == "" {
		return "gguf"
	}
	return f
}

func statusOf(resp *routing.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}
