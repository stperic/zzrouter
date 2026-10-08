package server

import (
	"github.com/gin-gonic/gin"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
)

// ClusterDetectionMiddleware detects internal cluster requests and
// stashes a marker in the request context via clusternode.WithClusterInternal.
// Registered early in the middleware chain (before authentication) so
// downstream gates can differentiate coordinator→worker dispatches from
// external client traffic.
func ClusterDetectionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader(clusternode.ClusterInternalHeader) == "true" {
			ctx := clusternode.WithClusterInternal(c.Request.Context())
			c.Request = c.Request.WithContext(ctx)
		}
		c.Next()
	}
}
