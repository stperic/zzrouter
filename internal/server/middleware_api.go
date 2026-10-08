package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/stperic/zzrouter/pkg/utils"
)

// RequestIDMiddleware generates a unique request ID for each request.
// If the client sends an X-Request-ID header, it is reused; otherwise a new
// UUID v4 is generated. The ID is stored in the gin context as "request_id"
// and returned in the X-Request-ID response header.
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = uuid.New().String()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// APIVersionMiddleware sets the X-API-Version response header on every request.
func APIVersionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-API-Version", "v1")
		c.Next()
	}
}

// RejectAPIKeyMiddleware bounces any request that carries an X-API-Key
// header with a clean 400 Problem Details explaining that worker reads
// are unauthenticated and writes go through the coordinator. Guards
// against:
//   - operators pointing a coord-era client at a worker and assuming
//     auth worked because the read succeeded;
//   - admin keys leaking into worker request logs where they don't
//     belong (coord is the single auth boundary — see plan §5.2).
func RejectAPIKeyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("X-API-Key") == "" {
			c.Next()
			return
		}
		problem := utils.NewProblemDetails(
			http.StatusBadRequest,
			"Do Not Send API Key to Worker",
			"workers do not accept X-API-Key: read-only management routes are unauthenticated; management writes go through the coordinator",
			c.Request.URL.Path,
		)
		if reqID, exists := c.Get("request_id"); exists {
			if id, ok := reqID.(string); ok {
				problem.RequestID = id
			}
		}
		c.Header("Content-Type", "application/problem+json")
		c.AbortWithStatusJSON(http.StatusBadRequest, problem)
	}
}
