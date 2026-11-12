package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/quota"
)

const browserBindingContext = "browser-credential-binding"

// Browser projections deliberately omit credential fingerprints, key metadata
// and installation configuration. They describe existing policy, never enforce it.
type browserSessionState struct {
	oauth.SessionInfo
	Issuer   string `json:"issuer"`
	NodeName string `json:"node_name"`
}

type browserLimits struct {
	RPMLimit            int     `json:"rpm_limit"`
	TPMLimit            int     `json:"tpm_limit"`
	MaxParallelRequests int     `json:"max_parallel_requests"`
	SpendLimit          float64 `json:"spend_limit"`
	ResetPeriod         string  `json:"reset_period"`
	DefaultMaxTokens    int     `json:"default_max_tokens"`
}

func browserQuota(q quota.QuotaConfig) browserLimits {
	return browserLimits{RPMLimit: q.RPMLimit, TPMLimit: q.TPMLimit, MaxParallelRequests: q.MaxParallelRequests,
		SpendLimit: q.SpendLimit, ResetPeriod: q.ResetPeriod, DefaultMaxTokens: q.DefaultMaxTokens}
}

type browserTeam struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	AllowedModels []string      `json:"allowed_models"`
	Limits        browserLimits `json:"limits"`
}

type browserIdentity struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Role      UserRole          `json:"role"`
	TeamID    string            `json:"team_id,omitempty"`
	IsVirtual bool              `json:"is_virtual"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
	Limits    browserLimits     `json:"limits"`
	Team      *browserTeam      `json:"team"`
	Usage     *KeyUsageResponse `json:"usage"`
}

func (s *Server) browserState(info oauth.SessionInfo) browserSessionState {
	return browserSessionState{SessionInfo: info, Issuer: s.access.oauth.Issuer(), NodeName: s.GetNodename()}
}

// requestAccess resolves credentials once before the existing role gate. Browser
// fallback is opt-in per route group and never overrides any explicit header.
func (a *AccessControl) requestAccess(c *gin.Context, browser bool, role string) (*AccessContext, bool) {
	cred := extractCredential(c, a.config)
	if cred.found() {
		ac, err := a.authenticateCredential(c, cred)
		if err == nil {
			return ac, true
		}
		stashRejectedKeyFingerprint(c, cred.key)
		a.rejectAuth(c, http.StatusUnauthorized, fmt.Sprintf("Invalid %sAPI key (presented via %s)", rolePrefix(role), cred.scheme))
		return nil, false
	}
	if browser && a.oauth != nil && !explicitCredential(c.Request) {
		ref, _, err := a.oauth.BrowserSession(c.Request.Context(), c.Request)
		if err == nil {
			if !safeBrowserMethod(c.Request.Method) {
				if err := a.oauth.CheckBrowserMutation(c.Request); err != nil {
					a.rejectAuth(c, http.StatusForbidden, "Invalid browser origin or CSRF proof")
					return nil, false
				}
			}
			ac, err := a.core.AuthenticateReference(ref)
			if err == nil {
				c.Set(browserBindingContext, ref)
				c.Header("Cache-Control", "no-store")
				return ac, true
			}
		}
	}
	a.rejectAuth(c, http.StatusUnauthorized, missingCredentialDetail(c))
	return nil, false
}

func rolePrefix(role string) string {
	if role == "" {
		return ""
	}
	return role + " "
}

func explicitCredential(r *http.Request) bool {
	for _, name := range nodeAPIKeyHeaders {
		if len(r.Header.Values(name)) > 0 {
			return true
		}
	}
	return false
}

func safeBrowserMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// browserAuthMiddleware uses the same identity/role pipeline as API keys, with
// cookie authentication enabled only for the management routes that opt in.
func (a *AuthHandlers) browserAuthMiddleware(role UserRole) gin.HandlerFunc {
	return func(c *gin.Context) {
		if a.access.authenticate(c, role, true) {
			c.Next()
		}
	}
}

func (s *Server) registerBrowserSessionRoutes() {
	session := s.engine.Group("/zzrouter/v1", RequestIDMiddleware(), APIVersionMiddleware(), s.browserCoordinatorOnly, browserResponseHeaders)
	session.GET("/session", s.handleBrowserBootstrap)
	session.POST("/session", s.handleBrowserLogin)
	session.DELETE("/session", s.handleBrowserLogout)
	self := session.Group("/me", func(c *gin.Context) {
		if s.access.authenticateMultiRole(c, []UserRole{RoleAdmin, RoleUser}, true) {
			c.Next()
		}
	})
	self.GET("", s.handleBrowserMe)
	self.GET("/connections", s.handleBrowserConnections)
	self.DELETE("/connections/:id", s.handleBrowserRevoke)
}

// Mounted routes retain their handlers across live role transitions. Gate them
// on the current role rather than destroying durable authority on demotion.
func (s *Server) browserCoordinatorOnly(c *gin.Context) {
	if s.role.Current().IsCoordinator() {
		c.Next()
		return
	}
	if !workerManagementMisdirected(c, s) {
		c.Status(http.StatusNotFound)
	}
	c.Abort()
}

func browserResponseHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "same-origin")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Next()
}

func (s *Server) handleBrowserBootstrap(c *gin.Context) {
	info, err := s.access.oauth.Bootstrap(c.Request.Context(), c.Writer, c.Request)
	if err != nil {
		browserSessionError(c, err)
		return
	}
	respondRetrievalSuccess(c, "Browser session", s.browserState(info))
}

func (s *Server) handleBrowserLogin(c *gin.Context) {
	// Only this credential field is accepted, and decoding never includes an
	// upstream error or the submitted value in a response or log.
	body, err := oauth.ReadBody(c.Writer, c.Request, 8<<10)
	if err != nil {
		BadRequest(c, "Could not read the browser sign-in request")
		return
	}
	var input struct {
		Key string `json:"key"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		BadRequest(c, "Expected a browser sign-in key")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		BadRequest(c, "Expected one sign-in request")
		return
	}
	info, err := s.access.oauth.Login(c.Request.Context(), c.Writer, c.Request, input.Key)
	if err != nil {
		browserSessionError(c, err)
		return
	}
	respondSuccess(c, "Signed in", s.browserState(info))
}

func (s *Server) handleBrowserLogout(c *gin.Context) {
	if err := s.access.oauth.Logout(c.Request.Context(), c.Writer, c.Request); err != nil {
		browserSessionError(c, err)
		return
	}
	respondSuccess(c, "Signed out", nil)
}

func (s *Server) handleBrowserMe(c *gin.Context) {
	ac := GetAccessContext(c)
	info := browserIdentity{ID: ac.Key.ID, Name: ac.Key.Name, Role: ac.Key.Role,
		IsVirtual: ac.Key.IsVirtual, ExpiresAt: ac.Key.ExpiresAt, Limits: browserQuota(ac.Key.Quotas)}
	if ac.Team != nil {
		info.TeamID = ac.Team.ID
		info.Team = &browserTeam{ID: ac.Team.ID, Name: ac.Team.Name,
			AllowedModels: append([]string{}, ac.Team.AllowedModels...), Limits: browserQuota(ac.Team.Quotas)}
	}
	if ac.Key.IsVirtual && s.services.Keys != nil {
		info.Usage, _ = s.services.Keys.GetKeyUsage(ac.Key.ID)
	}
	respondRetrievalSuccess(c, "Current identity", info)
}

func (s *Server) selfBinding(c *gin.Context) (control.CredentialReference, error) {
	if value, ok := c.Get(browserBindingContext); ok {
		if ref, ok := value.(control.CredentialReference); ok {
			return ref, nil
		}
	}
	return s.access.core.Reference(extractCredential(c, s.config).key)
}

func (s *Server) handleBrowserConnections(c *gin.Context) {
	ref, err := s.selfBinding(c)
	if err != nil {
		browserSessionError(c, oauth.ErrUnauthenticated)
		return
	}
	connections, err := s.access.oauth.Connections(c.Request.Context(), ref)
	if err != nil {
		browserSessionError(c, err)
		return
	}
	respondList(c, connections, len(connections), false)
}

func (s *Server) handleBrowserRevoke(c *gin.Context) {
	ref, err := s.selfBinding(c)
	if err != nil {
		browserSessionError(c, oauth.ErrUnauthenticated)
		return
	}
	if err := s.access.oauth.RevokeConnection(c.Request.Context(), ref, c.Param("id")); err != nil {
		browserSessionError(c, err)
		return
	}
	respondSuccess(c, "Connection revoked", nil)
}

func browserSessionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, oauth.ErrCSRF), errors.Is(err, oauth.ErrAmbiguousCookie):
		Forbidden(c, "Invalid browser origin or CSRF proof")
	case errors.Is(err, oauth.ErrUnauthenticated):
		Unauthorized(c, "Browser sign-in required")
	case errors.Is(err, oauth.ErrLoginLimited):
		RespondWithProblem(c, http.StatusTooManyRequests, "Too Many Requests", "Browser sign-in temporarily limited")
	default:
		RespondWithProblem(c, http.StatusServiceUnavailable, "Service Unavailable", "Could not save browser authorization. Please retry.")
	}
}
