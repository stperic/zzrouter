package server

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/inferencelog"
)

// historyDefaultLimit caps a single /history page when ?limit is absent
// or invalid. The inferencelog store already enforces an upper bound;
// this just normalizes the public surface.
const historyDefaultLimit = 50

// historyEntry is the projected wire shape for /history. Carries only
// the fields useful to a routing agent — request body, message contents,
// and internal IDs are intentionally omitted. Add fields as concrete
// agent use cases emerge; don't bloat the wire speculatively.
type historyEntry struct {
	ID             string  `json:"id"`
	Timestamp      string  `json:"timestamp"`
	Model          string  `json:"model"`
	ResponseModel  string  `json:"response_model,omitempty"`
	Status         string  `json:"status"`
	DeploymentName string  `json:"replica,omitempty"`
	FallbackCount  int     `json:"fallback_count"`
	FallbackFrom   string  `json:"fallback_from,omitempty"`
	LatencyMs      float64 `json:"latency_ms,omitempty"`
	TokensIn       int64   `json:"tokens_in,omitempty"`
	TokensOut      int64   `json:"tokens_out,omitempty"`
	CostUSD        float64 `json:"cost_usd,omitempty"`
	ErrorType      string  `json:"error_type,omitempty"`
}

type modelGroupHistoryResponse struct {
	Name    string         `json:"name"`
	Entries []historyEntry `json:"entries"`
	Count   int            `json:"count"`
}

// GetModelGroupHistory handles GET /zzrouter/v1/model-groups/:name/history.
// Query params:
//
//	?since=<RFC3339>  — entries newer than this timestamp
//	?limit=<int>      — page size; default 50
//
// Filtering is on GroupName == :name (NOT FallbackCount > 0). A
// successful single-attempt dispatch records FallbackCount=0 — those
// rows are still legitimate route history and must be included; the
// caller learns "did this involve fallback" from FallbackCount on the
// returned row, not from the filter.
func (ctrl *ModelGroupsController) GetModelGroupHistory(c *gin.Context) {
	name := c.Param("name")
	if ctrl.groupStore.Get(name) == nil {
		NotFound(c, "model group not found")
		return
	}
	if ctrl.logStore == nil {
		ServiceUnavailable(c, "inference log store not configured on this node")
		return
	}

	filter := inferencelog.QueryFilter{
		GroupName: name,
		Limit:     historyDefaultLimit,
	}
	if raw := c.Query("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			BadRequest(c, "invalid ?since: must be RFC3339, e.g. 2026-05-12T00:00:00Z")
			return
		}
		filter.Since = t
	}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			BadRequest(c, "invalid ?limit: must be a positive integer")
			return
		}
		filter.Limit = n
	}

	rows := ctrl.logStore.Query(filter)
	entries := make([]historyEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, historyEntry{
			ID:             r.ID,
			Timestamp:      r.Timestamp.UTC().Format(time.RFC3339Nano),
			Model:          r.Model,
			ResponseModel:  r.ResponseModel,
			Status:         r.Status,
			DeploymentName: r.DeploymentName,
			FallbackCount:  r.FallbackCount,
			FallbackFrom:   r.FallbackFrom,
			LatencyMs:      r.LatencyMs,
			TokensIn:       r.TokensIn,
			TokensOut:      r.TokensOut,
			CostUSD:        r.Cost,
			ErrorType:      r.ErrorType,
		})
	}

	c.Header("Cache-Control", "no-store")
	respondSuccess(c, "Model group history retrieved successfully", modelGroupHistoryResponse{
		Name:    name,
		Entries: entries,
		Count:   len(entries),
	})
}
