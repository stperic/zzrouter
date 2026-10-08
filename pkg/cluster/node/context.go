package clusternode

import "context"

// Cluster-internal routing headers. Coordinator→worker dispatches
// stamp these on outbound requests; ClusterDetectionMiddleware on
// the worker reads them back and stashes the value via WithClusterInternal.
const (
	// ClusterInternalHeader marks a request as originating from
	// another cluster node rather than an external client. Auth paths
	// branch on this to substitute the cluster network key for the
	// admin key.
	ClusterInternalHeader = "X-Cluster-Internal"

	// ClusterAPIKeyHeader carries the cluster network key on
	// coordinator→worker dispatches (not stored in context; read once
	// at the auth boundary and discarded).
	ClusterAPIKeyHeader = "X-Cluster-API-Key" //nolint:gosec // header name, not a credential
)

type clusterInternalKey struct{}

// WithClusterInternal marks ctx as carrying a cluster-internal
// request (X-Cluster-Internal: true was present on the inbound
// request). Used by the server's ClusterDetectionMiddleware.
func WithClusterInternal(ctx context.Context) context.Context {
	return context.WithValue(ctx, clusterInternalKey{}, true)
}

// IsClusterInternal reports whether ctx carries the cluster-internal
// marker. Takes a context.Context rather than *gin.Context so the
// function can be called from any layer — the old gin-context
// signature was misleading since the implementation only read
// c.Request.Context() anyway.
func IsClusterInternal(ctx context.Context) bool {
	val, ok := ctx.Value(clusterInternalKey{}).(bool)
	return ok && val
}
