// compat_middleware.go — middleware specific to the third-party-API
// compatibility surfaces (/v1/* OpenAI, /api/* Ollama).
//
// OptionalAuthMiddleware — accepts the request whether or not an API
// key is present (so anonymous SDK callers work) but validates and
// attaches access context when one IS provided. Hardens to mandatory
// auth via Auth.RequireCompatAuth.
//
// Worker-side gating: the legacy header-gated middleware was retired
// when worker compat moved to the cluster mTLS port. mTLS + OU=coord
// at the transport layer IS the gate; the worker compat engine's
// clusterTrustedMiddleware sets CtxKeyClusterTrusted so OptionalAuth
// below skips API-key re-validation for coord-proxied traffic.
package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// OptionalAuthMiddleware creates middleware that validates a key if present,
// but allows unauthenticated requests through (unless RequireCompatAuth is set).
//
// Cluster-trusted requests (cluster mTLS port, set by clusterTrustedMiddleware
// on the worker compat engine) get a synthetic admin context so a forwarded
// X-API-Key from the original coord-side caller doesn't fail validation
// against the worker's own keys.
func (a *AuthHandlers) OptionalAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if trusted, _ := c.Get(string(CtxKeyClusterTrusted)); trusted == true {
			if a.access != nil {
				a.access.setSyntheticContext(c, RoleAdmin, "cluster-proxy")
			}
			c.Next()
			return
		}

		cred := extractCompatCredential(c)
		if !cred.found() {
			a.allowAnonymous(c)
			return
		}
		apiKey := cred.key

		if a.access == nil {
			c.Next()
			return
		}

		ac, err := a.access.AuthenticateRaw(apiKey)
		if err != nil {
			// A client whose key field is cleared often still sends a
			// header: the empty config serializes as "null" or
			// "undefined" rather than being omitted. That carries the
			// same intent as no credential, so route it to the
			// anonymous branch — which still refuses when
			// RequireCompatAuth is on. Checked only after
			// authentication has failed, so a deployment that really
			// issued one of these strings as a key keeps working.
			if isPlaceholderCredential(apiKey) {
				a.allowAnonymous(c)
				return
			}
			stashRejectedKeyFingerprint(c, apiKey)
			a.rejectAuth(c, "Invalid API key")
			return
		}

		setAccessContext(c, ac)
		c.Next()
	}
}

// allowAnonymous admits a request that presented no usable credential,
// or refuses it when the compat surface has been hardened to mandatory
// auth. Single home for that decision so the empty-header path and the
// placeholder path can never drift apart.
func (a *AuthHandlers) allowAnonymous(c *gin.Context) {
	if why := anonymousRefusal(a.config, a.access); why != "" {
		a.rejectAuth(c, missingCredentialDetail(c)+" ("+why+")")
		return
	}
	c.Next()
}

// anonymousRefusal says why this node refuses a request that carries no
// key, or "" when it admits one. It is the one rule: the middleware
// enforces it on every surface it guards, the discovery document reports
// it, and the startup warning reads it.
func anonymousRefusal(cfg *pkgConfig.NodeConfig, access *AccessControl) string {
	if cfg != nil && cfg.Auth.RequireCompatAuth {
		return "this node sets auth.require_compat_auth"
	}
	// An anonymous caller cannot be held to a team's model allow-list.
	if access.HasGatedTeam() {
		return "a team on this node limits which models its keys may use"
	}
	return ""
}

// placeholderCredentials holds the values clients emit for "no API
// key" when the field is cleared but the header still goes out. Kept
// as a closed set rather than a prefix or length rule so widening it
// is always a deliberate edit.
var placeholderCredentials = map[string]struct{}{
	"":                   {},
	"null":               {},
	"nil":                {},
	"none":               {},
	"undefined":          {},
	"empty":              {},
	"no-key":             {},
	"nokey":              {},
	"no-key-required":    {},
	"no-key-needed":      {},
	"not-needed":         {},
	"unused":             {},
	"dummy":              {},
	"placeholder":        {},
	"sk-none":            {},
	"sk-no-key-required": {},
	"ollama":             {},
	"lm-studio":          {},
}

// isPlaceholderCredential reports whether a presented key is one of the
// well-known stand-ins for "I have no key".
func isPlaceholderCredential(key string) bool {
	_, ok := placeholderCredentials[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

// rejectAuth emits a 401 in the active dialect and aborts the gin chain.
func (a *AuthHandlers) rejectAuth(c *gin.Context, detail string) {
	logAuthAttempt(c, a.config, http.StatusUnauthorized, detail)
	r := httperr.FromContextOr(c, a.responders.problem)
	r.Unauthorized(c, detail)
	c.Abort()
}

// clusterHeadersFromCoordinator honours what a coordinator says about a
// request it proxies. Like clusterTrustedMiddleware it is installed only
// on the worker's cluster engine, behind the mTLS + OU=coordinator gate.
func clusterHeadersFromCoordinator() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader(headerNotInference) != "" {
			markNotInference(c)
		}
		c.Next()
	}
}

// dropCoordinatorHeaders removes the headers only a coordinator may send,
// so a client cannot pass one through a node that forwards its headers.
func dropCoordinatorHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Header.Del(headerNotInference)
		for _, h := range constants.RoutingHintHeaders() {
			c.Request.Header.Del(h)
		}
		c.Next()
	}
}
