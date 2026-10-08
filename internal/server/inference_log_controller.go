// package server provides HTTP handlers for the zzrouter host server.
// InferenceLogController - HTTP handlers for inference log endpoints.
// Real-time streaming lives at /zzrouter/v1/jobs/:id/stream (the singleton
// inference_log firehose, discoverable via GET /jobs?kind=inference_log).
// /inference-logs/stream is a discoverability shim — it returns the
// firehose URL so agents can find the SSE endpoint without scanning
// /jobs.

package server

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/utils"
)

// InferenceLogController handles HTTP requests for inference log endpoints.
type InferenceLogController struct {
	service *InferenceLogService
	// logJobIDFn returns the singleton inference_log firehose job ID.
	// Lazy lookup so /inference-logs/stream works even if the handle
	// gets replaced (currently it never is — the handle is created
	// once at server boot).
	logJobIDFn func() string
}

// NewInferenceLogController creates a new inference log controller.
// logJobIDFn returns the inference_log firehose job ID (server-level
// state). Pass nil if firehose isn't wired — the /stream endpoint
// will respond with a 503-style Problem Details.
func NewInferenceLogController(service *InferenceLogService, logJobIDFn func() string) *InferenceLogController {
	return &InferenceLogController{service: service, logJobIDFn: logJobIDFn}
}

// RegisterPublicRoutes registers inference log routes on the public API group.
// Order matters: /inference-logs/stream must come BEFORE /inference-logs/:id
// or gin matches "stream" as the :id param and the discovery shim 404s.
func (ctrl *InferenceLogController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.GET("/inference-logs", ctrl.ListLogs)
	router.GET("/inference-logs/stream", ctrl.GetStreamPointer)
	router.GET("/inference-logs/:id", ctrl.GetLog)
	router.GET("/inference-logs/:id/payload", ctrl.GetPayload)

	// Per-model usage since node start. Attribution-free: these totals come
	// off the request record itself, so they need no virtual key and no team
	// — which is what makes them meaningful for a locally deployed model
	// that nobody authenticates against.
	router.GET("/usage/models", ctrl.ListModelUsage)
	router.GET("/usage/models/:model", ctrl.GetModelUsage)
}

// ListModelUsage handles GET /zzrouter/v1/usage/models.
// Returns running per-model usage for every model seen since node start.
func (ctrl *InferenceLogController) ListModelUsage(c *gin.Context) {
	report, _ := ctrl.service.ModelUsage("", "")
	respondSuccess(c, "Model usage retrieved", report)
}

// GetModelUsage handles GET /zzrouter/v1/usage/models/:model.
//
// Optional ?provider= scopes to one deployment. Without it, a model name
// served by several providers is summed, which is what a caller asking about
// "the model" rather than "this deployment" wants.
//
// Cost is reported only when at least one request carried an authoritative
// price. A self-hosted model has no pricing, and rendering it as $0.00 would
// read as broken rather than as free — so the cost field is omitted and
// priced=false says why.
func (ctrl *InferenceLogController) GetModelUsage(c *gin.Context) {
	model := c.Param("model")
	report, ok := ctrl.service.ModelUsage(c.Query("provider"), model)
	if !ok {
		NotFound(c, "no usage recorded for model "+model+" since node start")
		return
	}
	respondSuccess(c, "Model usage retrieved", report)
}

// GetStreamPointer returns the firehose URL so agents can discover the
// SSE endpoint without a separate /jobs lookup. This is a discoverability
// shim — the actual streaming lives at /jobs/:id/stream.
func (ctrl *InferenceLogController) GetStreamPointer(c *gin.Context) {
	if ctrl.logJobIDFn == nil {
		ServiceUnavailable(c, "inference-log firehose is not enabled on this node")
		return
	}
	jobID := ctrl.logJobIDFn()
	if jobID == "" {
		ServiceUnavailable(c, "inference-log firehose has no active handle")
		return
	}
	respondSuccess(c, "Inference-log firehose pointer", gin.H{
		"stream_url": "/zzrouter/v1/jobs/" + jobID + "/stream",
		"job_id":     jobID,
		"kind":       "inference_log",
		"note":       "Subscribe with the same X-API-Key header. Each event's Meta.entry carries the full LogEntry.",
	})
}

// ListLogs returns inference log entries matching optional filters.
//
// Query params: model, status, key_id, team_id, since (duration like
// "5m" or RFC3339), limit, offset.
func (ctrl *InferenceLogController) ListLogs(c *gin.Context) {
	status := c.Query("status")
	if !ValidateEnum(c, "status", status, InferenceLogStatuses) {
		return
	}

	filter := inferencelog.QueryFilter{
		Model:  QueryModel(c),
		Status: status,
		KeyID:  c.Query("key_id"),
		TeamID: c.Query("team_id"),
	}

	if since := c.Query("since"); since != "" {
		filter.Since = parseSince(since)
	}

	pagination, ok := ParsePagination(c)
	if !ok {
		return
	}
	filter.Limit = pagination.Limit
	filter.Offset = pagination.Offset

	entries := ctrl.service.Query(filter)
	if entries == nil {
		entries = []inferencelog.LogEntry{}
	}

	total := ctrl.service.Len()
	hasMore := pagination.Offset+len(entries) < total
	respondList(c, entries, total, hasMore)
}

// GetLog returns a single inference log entry by ID.
func (ctrl *InferenceLogController) GetLog(c *gin.Context) {
	id := c.Param("id")
	entry, ok := ctrl.service.Get(id)
	if !ok {
		NotFound(c, "inference log entry not found")
		return
	}
	respondSuccess(c, "Inference log entry retrieved", entry)
}

// GetPayload returns the full request bodies retained for an entry.
// Retention covers a shorter window than the metadata ring, so an entry
// that exists can still 404 here once its bodies have aged out.
func (ctrl *InferenceLogController) GetPayload(c *gin.Context) {
	id := c.Param("id")
	if _, exists := ctrl.service.Get(id); !exists {
		NotFound(c, "inference log entry not found")
		return
	}
	payload, ok := ctrl.service.Payload(id)
	if !ok {
		NotFound(c, "payload no longer retained for this entry")
		return
	}
	respondSuccess(c, "Inference log payload retrieved", payload)
}

// parseSince parses a "since" parameter as either a duration ("5m", "1h") or RFC3339 timestamp.
func parseSince(s string) time.Time {
	// Try duration first (e.g., "5m", "1h", "30s")
	if d, err := time.ParseDuration(s); err == nil {
		return utils.Now().Add(-d)
	}
	// Try RFC3339 timestamp
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
