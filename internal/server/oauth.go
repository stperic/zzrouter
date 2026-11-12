package server

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
	"github.com/stperic/zzrouter/pkg/access/control"
)

const (
	adminMCPPath             = "/zzrouter/v1/mcp"
	inferenceMCPPath         = "/inference/mcp"
	browserSessionCookie     = "zzrouter_oauth_session"
	browserCSRFCookie        = "zzrouter_oauth_browser"
	browserHostSessionCookie = "__Host-" + browserSessionCookie
	browserHostCSRFCookie    = "__Host-" + browserCSRFCookie
)

func (s *Server) initOAuth() error {
	if s.node.IsWorker() {
		return nil
	}
	issuer, err := s.config.Auth.OAuth.EffectiveIssuer(s.config.Node)
	if err != nil {
		return err
	}
	if issuer == "" {
		return nil
	}
	browser := oauth.BrowserOptions{SessionCookie: browserSessionCookie, CSRFCookie: browserCSRFCookie, LegacyPath: "/oauth"}
	if strings.HasPrefix(issuer, "https://") {
		browser = oauth.BrowserOptions{SessionCookie: browserHostSessionCookie, CSRFCookie: browserHostCSRFCookie}
	}
	views, err := parseBrowserViews()
	if err != nil {
		return fmt.Errorf("parse browser views: %w", err)
	}
	authority, err := oauth.New(oauth.Options[control.CredentialReference]{
		Issuer: issuer, StatePath: filepath.Join(s.auditConfigDir, "oauth.json"),
		Resources: []oauth.Resource{{Path: adminMCPPath, Scope: "zzrouter:manage"}, {Path: inferenceMCPPath, Scope: "zzrouter:inference"}},
		Browser:   browser,
		Authenticate: func(_ context.Context, key string) (control.CredentialReference, error) {
			return s.access.core.Reference(key)
		},
		Resolve: func(_ context.Context, ref control.CredentialReference) (oauth.IdentityView, error) {
			if !s.role.Current().IsCoordinator() {
				return oauth.IdentityView{}, control.ErrInvalidKey
			}
			ac, err := s.access.core.AuthenticateReference(ref)
			if err != nil {
				return oauth.IdentityView{}, err
			}
			return oauth.IdentityView{ID: ac.Key.ID, Name: ac.Key.Name}, nil
		},
		Authorize: func(_ context.Context, ref control.CredentialReference, resource oauth.Resource) error {
			role := control.RoleUser
			if resource.Path == adminMCPPath {
				role = control.RoleAdmin
			}
			if !control.RoleAllowed(ref.Role, role) {
				return control.ErrInvalidKey
			}
			return nil
		},
		Render: func(w io.Writer, page oauth.Page) error {
			return views.ExecuteTemplate(w, "oauth.gohtml", page)
		},
	})
	if err != nil {
		return fmt.Errorf("initialize MCP browser login: %w", err)
	}
	s.access.oauth = authority
	return nil
}
func (s *Server) registerOAuthRoutes() {
	if s.access.oauth == nil {
		return
	}
	s.registerBrowserSessionRoutes()
	serve := gin.WrapH(s.access.oauth)
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource" + adminMCPPath, "/.well-known/oauth-protected-resource" + inferenceMCPPath, "/oauth/authorize", "/oauth/connections"} {
		s.engine.GET(path, s.browserCoordinatorOnly, serve)
	}
	for _, path := range []string{"/oauth/authorize", "/oauth/connections", "/oauth/logout", "/oauth/token", "/oauth/register", "/oauth/revoke"} {
		s.engine.POST(path, s.browserCoordinatorOnly, serve)
	}
	for _, path := range []string{"/oauth/token", "/oauth/register", "/oauth/revoke"} {
		s.engine.OPTIONS(path, s.browserCoordinatorOnly, serve)
	}
}

// authenticateCredential preserves the credential's kind and resource boundary.
func (a *AccessControl) authenticateCredential(c *gin.Context, cred credential) (*AccessContext, error) {
	path := c.Request.URL.Path
	if dispatched := mcpserver.ResourcePath(c.Request.Context()); dispatched != "" {
		path = dispatched
	}
	// Native routes do not depend on OAuth transactions or their disk writes.
	if path != adminMCPPath && path != inferenceMCPPath || a.oauth == nil || !a.oauth.OwnsAccessToken(cred.key) {
		return a.core.Authenticate(cred.key)
	}
	if cred.scheme != authorizationHeader+": "+bearerScheme {
		return nil, control.ErrInvalidKey
	}
	ref, err := a.oauth.Authenticate(c.Request.Context(), cred.key, a.oauth.Issuer()+path)
	if err != nil {
		return nil, err
	}
	ac, err := a.core.AuthenticateReference(ref)
	if err == nil {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), oauthCredentialKey{}, true))
	}
	return ac, err
}
func (a *AccessControl) oauthChallenge(c *gin.Context) {
	if a.oauth == nil {
		return
	}
	path := c.Request.URL.Path
	if dispatched := mcpserver.ResourcePath(c.Request.Context()); dispatched != "" {
		path = dispatched
	}
	if challenge := a.oauth.Challenge(path); challenge != "" {
		c.Header("WWW-Authenticate", challenge)
	}
}

type oauthCredentialKey struct{}

func isOAuthRequest(ctx context.Context) bool {
	value, _ := ctx.Value(oauthCredentialKey{}).(bool)
	return value
}
