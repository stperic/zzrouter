package server

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
