package server

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/audit"
)

// auditPage is one page of the access history, newest first.
type auditPage struct {
	Events []audit.Event `json:"events"`
	Next   string        `json:"next,omitempty"`
}

func (s *Server) handleAccessAudit(c *gin.Context) {
	limit := 50
	if value := c.Query("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			BadRequest(c, "Limit must be between 1 and 100")
			return
		}
		limit = parsed
	}
	events, next, err := s.accessStore.AuditPage(c.Request.Context(), c.Query("cursor"), limit)
	if errors.Is(err, audit.ErrCursor) {
		Conflict(c, err.Error())
		return
	}
	if err != nil {
		if !respondKnownFailure(c, err) {
			ServiceUnavailable(c, "Could not read audit history")
		}
		return
	}
	respondRetrievalSuccess(c, "Audit history", auditPage{Events: events, Next: next})
}
