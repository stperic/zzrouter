package server

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/audit"
)

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
	s.auditMu.RLock()
	defer s.auditMu.RUnlock()
	sink, ok := s.auditSink.(*audit.FileSink)
	if !ok {
		ServiceUnavailable(c, "Audit recording is unavailable on this coordinator")
		return
	}
	page, err := sink.ReadPage(c.Request.Context(), c.Query("cursor"), limit)
	if errors.Is(err, audit.ErrCursor) {
		Conflict(c, err.Error())
		return
	}
	if err != nil {
		ServiceUnavailable(c, "Could not read audit history")
		return
	}
	respondRetrievalSuccess(c, "Audit history", page)
}
