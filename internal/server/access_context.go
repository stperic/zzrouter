// package server — gin adapter for AccessContext storage.
// AccessContext, KeyPrincipal, TeamPrincipal moved to pkg/access/control;
// this file carries only the gin-request-scoped accessors.

package server

import (
	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/proxy"
)

// GetAccessContext retrieves the AccessContext from a gin context.
// Returns nil if no context was set (anonymous request) or the gin
// context itself is nil (test paths).
func GetAccessContext(c *gin.Context) *AccessContext {
	if c == nil {
		return nil
	}
	val, exists := c.Get(string(CtxKeyAccessContext))
	if !exists {
		return nil
	}
	ac, _ := val.(*AccessContext)
	return ac
}

// PrincipalFromContext returns a stable identifier for the authenticated
// caller — virtual-key ID when present, otherwise the admin-key
// fingerprint, otherwise "admin" for pre-virtual-key admin-only paths,
// otherwise "" for anonymous. Producers opening a jobs.Handle pass this
// as createdBy so CreatedBy attribution survives every later authz
// refactor.
func PrincipalFromContext(c *gin.Context) string {
	ac := GetAccessContext(c)
	if ac == nil || ac.Key == nil {
		return ""
	}
	if ac.Key.ID != "" {
		return ac.Key.ID
	}
	if ac.Key.Fingerprint != "" {
		return ac.Key.Fingerprint
	}
	return "admin"
}

// setAccessContext stores the AccessContext on a gin context. Also sets
// the audit fields consumed by request-logging middleware: the typed
// keys live in types.go so readers can import the constant instead of
// guessing the string casing.
//
// Surfaces the same identity onto the proxy-metric stash so
// zz.proxy.failed.requests.metric carries authenticated-caller labels
// (api_key_alias / hashed_api_key / team / team_alias) on the failure
// series for any 4xx/5xx fired downstream of authentication. Empty
// values for unauthenticated callers fall through to empty-label
// series — distinguishable from authenticated failures by the empty
// strings.
func setAccessContext(c *gin.Context, ac *AccessContext) {
	c.Set(string(CtxKeyAccessContext), ac)
	if ac == nil || ac.Key == nil {
		return
	}
	c.Set(string(CtxKeyUserRole), ac.Key.Role)
	c.Set(string(CtxKeyAPIKeyFingerprint), ac.Key.Fingerprint)
	c.Set(string(CtxKeyAuthenticated), true)
	c.Set(proxy.CtxKeyAPIKeyAlias, ac.Key.Name)
	c.Set(proxy.CtxKeyHashedAPIKey, ac.Key.Fingerprint)
	if ac.Team != nil {
		c.Set(proxy.CtxKeyTeamID, ac.Team.ID)
		c.Set(proxy.CtxKeyTeamAlias, ac.Team.Name)
	}
}

// mirrorCallerIdentityToRecorder copies the LiteLLM-vocabulary caller
// labels (api_key_alias, hashed_api_key, team_alias) from gin.Context
// onto the InferenceRecorder. Called from every recorder constructor
// (newChatRecorder, newCompletionRecorder, newEmbeddingsRecorder,
// newOllamaInferenceRecorder) so the post-response inference-log
// bridge can emit zz.input.tokens.metric / zz.output.tokens.metric
// with populated labels regardless of which dispatch surface the
// request flowed through. Reading from the gin keys (already
// populated by setAccessContext for both virtual and static keys)
// keeps the helper independent of AccessContext's shape.
//
// Static admin/user keys carry their configured Name; static keys
// without a Name fall back to the empty string. Anonymous traffic on
// the /v1/* + /api/* surfaces is rejected before the recorder runs,
// so the empty-label series are reserved for early-init paths and
// trusted internal-request bypass — both expected to be a tiny
// fraction of traffic.
func mirrorCallerIdentityToRecorder(c *gin.Context, recorder *llm.InferenceRecorder) {
	if c == nil || recorder == nil {
		return
	}
	alias, _ := c.Get(proxy.CtxKeyAPIKeyAlias)
	hashed, _ := c.Get(proxy.CtxKeyHashedAPIKey)
	teamAlias, _ := c.Get(proxy.CtxKeyTeamAlias)
	aliasS, _ := alias.(string)
	hashedS, _ := hashed.(string)
	teamAliasS, _ := teamAlias.(string)
	recorder.SetCallerIdentity(aliasS, hashedS, teamAliasS)
}
