package server

import (
	"log/slog"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	obsproxy "github.com/stperic/zzrouter/pkg/observability/proxy"
)

// loopbackOrAdmin returns a middleware that skips the wrapped admin
// auth check when the request came from 127.0.0.1 / ::1. Used for
// endpoints where the only legitimate caller is the local CLI on the
// same machine — env-var coordination between the daemon's startup
// environment and the operator's shell is more friction than the
// admin gate is worth for localhost. Non-loopback callers still get
// the full admin check.
func loopbackOrAdmin(admin gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		if err != nil {
			// RemoteAddr is unparseable; fall back to admin auth
			// rather than fail open.
			admin(c)
			return
		}
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			c.Next()
			return
		}
		admin(c)
	}
}

// logAuthAttempt records a rejected authentication when
// security.log_auth_attempts is on. Both rejection choke points (the
// admin surface's AccessControl.rejectAuth and the compat surface's
// AuthHandlers.rejectAuth) route through here so the operator sees one
// shape regardless of which surface refused.
//
// The key itself is never logged. What lands in the record is the
// truncated fingerprint stashRejectedKeyFingerprint already computed
// for the metric label, which is absent when the caller sent no
// credential at all — "none" distinguishes a missing key from a
// rejected one, and those are different operator problems.
func logAuthAttempt(c *gin.Context, cfg *pkgConfig.NodeConfig, status int, detail string) {
	if cfg == nil || !cfg.Security.LogAuthAttempts {
		return
	}
	fingerprint := "none"
	if v, ok := c.Get(obsproxy.CtxKeyHashedAPIKey); ok {
		if f, ok := v.(string); ok && f != "" {
			fingerprint = f
		}
	}
	slog.Warn("auth attempt rejected",
		"status", status,
		"detail", detail,
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"client_ip", c.ClientIP(),
		"key_fingerprint", fingerprint)
}

// UserRole, RoleAdmin, RoleUser live in pkg/access/control and are
// re-exported through access_aliases.go for call-site brevity.

// AuthHandlers groups authentication middleware creation.
type AuthHandlers struct {
	access          *AccessControl
	config          *pkgConfig.NodeConfig
	nodeConfigStore *pkgConfig.NodeConfigStore
	responders      *responderSet
}

// AuthMiddleware creates role-based authentication middleware backed by the AccessControl.
func (a *AuthHandlers) AuthMiddleware(requiredRole UserRole) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !a.access.Authenticate(c, requiredRole) {
			return
		}
		c.Next()
	}
}

// AuthMiddlewareMultiRole creates authentication middleware that accepts multiple roles.
func (a *AuthHandlers) AuthMiddlewareMultiRole(acceptedRoles []UserRole) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !a.access.AuthenticateMultiRole(c, acceptedRoles) {
			return
		}
		c.Next()
	}
}

// adminAuthMiddleware validates admin API keys.
func (a *AuthHandlers) adminAuthMiddleware() gin.HandlerFunc {
	return a.AuthMiddleware(RoleAdmin)
}

// accessControlAuthMiddleware gates the access-control surface (teams, keys,
// spend). Currently identical to adminAuthMiddleware — split so operators can
// later swap in a dedicated ZZROUTER_ACCESS_CONTROL_API_KEY without touching
// any handler, URL, or client. The split is a seam, not a feature yet.
func (a *AuthHandlers) accessControlAuthMiddleware() gin.HandlerFunc {
	return a.AuthMiddleware(RoleAdmin)
}

// internalRequestOnlyMiddleware gates the public-engine /internal/*
// mount so only in-process dispatch (Server.ServeClusterRequest →
// s.engine.ServeHTTP) can reach the executor routes. External network
// callers get 404 — remote coordinator→worker dispatch is served on
// the mTLS cluster-port listener (pkg/clusternode).
//
// The marker (internalRequestKey) is set by ServeClusterRequest right
// after HTTP request construction. It cannot be spoofed from the
// network because context values are per-request and not serialized.
func internalRequestOnlyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Context().Value(internalRequestKey) != true {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// Compat-surface middleware (OptionalAuthMiddleware) lives in
// compat_middleware.go. Worker-side gating is now handled by mTLS on
// the cluster port (see worker_compat_engine.go).
