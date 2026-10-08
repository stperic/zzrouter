// package server — AccessControl: gin adapter over pkg/access/control.Core.
//
// Core owns the pure pipeline: authentication cache, authorizer, quota
// chain, reservation stash, reaper, spend ledger. This file is purely
// the HTTP adapter — it extracts keys from gin headers, writes the
// AccessContext onto gin context, translates Core's Decision into HTTP
// status codes + OpenAI error envelopes, and bridges the trusted-
// internal-request bypass. Callers in auth_middleware, model_dispatch,
// keys_service, teams_service, and inference_log_bridge call through
// this adapter; none reach past it into Core directly.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	obsproxy "github.com/stperic/zzrouter/pkg/observability/proxy"
	"github.com/stperic/zzrouter/pkg/utils"
)

// internalRequestKey is a context key for trusted in-process requests.
// Set by ServeClusterRequest to bypass auth for in-process cluster dispatch.
type internalRequestKeyType struct{}

var internalRequestKey = internalRequestKeyType{}

// AccessControl is the gin adapter wrapping a control.Core. The public
// method surface is stable: callers in auth_middleware.go,
// model_dispatch.go, keys_service.go, teams_service.go, and
// inference_log_bridge.go use these methods directly; Core is not
// exposed.
type AccessControl struct {
	core   *control.Core
	config *pkgConfig.NodeConfig
	hooks  ModelHooks
}

// ModelHooks are the model lookups the access layer needs but cannot
// compute itself: it holds no model cache and no provider config, and the
// server owns both. Every field is optional and the zero value is the
// pre-hook behaviour — set once at construction and only read afterwards,
// so request goroutines need no synchronisation to see them.
type ModelHooks struct {
	// IsCloudBacked reports whether a model resolves to a cloud-backed
	// provider. Nil reports every model as local, which leaves the
	// anonymous path exactly as it was before the cloud gate existed.
	IsCloudBacked func(ctx context.Context, model string) bool

	// Identity normalizes a model reference to the id the allow list is
	// compared on. Nil compares references verbatim.
	Identity control.ModelIdentity
}

// NewAccessControl constructs the adapter + its underlying Core. Caller
// must call Start() to begin background goroutines.
func NewAccessControl(
	staticKeys StaticKeySet,
	keyStore keys.KeyStore,
	teamStore teams.TeamStore,
	groups modelgroup.GroupReader,
	enforcer *quota.Enforcer,
	config *pkgConfig.NodeConfig,
	hooks ModelHooks,
) *AccessControl {
	if teamStore == nil {
		slog.Info("AccessControl constructed without a team store; team features disabled")
	}
	return &AccessControl{
		core:   control.NewCore(staticKeys, keyStore, teamStore, groups, enforcer, hooks.Identity),
		config: config,
		hooks:  hooks,
	}
}

// anonymousCloudDenied reports whether an unauthenticated request for
// this model must be refused because the model is cloud-backed.
//
// A cloud model is served with the operator's upstream credentials, so
// an anonymous call spends real money that no virtual key, budget, or
// spend record can be attributed to. Local models are unaffected: they
// cost nothing per request and stay reachable without a key, which is
// what keeps plain SDK clients working against a local node.
func (a *AccessControl) anonymousCloudDenied(ctx context.Context, modelName string) bool {
	if a.AnonymousCloudAllowed() {
		return false
	}
	return a.hooks.IsCloudBacked != nil && modelName != "" && a.hooks.IsCloudBacked(ctx, modelName)
}

// AnonymousCloudAllowed reports whether a keyless request may reach a
// cloud backend, which bills this node's upstream account.
func (a *AccessControl) AnonymousCloudAllowed() bool {
	return a == nil || a.config == nil || a.config.Auth.AllowAnonymousCloud
}

// Start / Stop forward to Core; callers must not wrap these in a `go`
// statement — Core owns its own goroutines.
func (a *AccessControl) Start(ctx context.Context) {
	if a == nil {
		return
	}
	a.core.Start(ctx)
}

func (a *AccessControl) Stop(ctx context.Context) {
	if a == nil {
		return
	}
	a.core.Stop(ctx)
}

// ===== Authentication =====

// Authenticate validates the request's API key and enforces role
// requirements. On success, stores AccessContext on gin context and
// returns true. On failure, writes an HTTP error and returns false.
//
// Order (pinned by design doc §"Adapter call flow"):
//  1. Trusted internal request (in-process) → bypass with required role.
//  2. Extract raw key from headers.
//  3. Call core.Authenticate — cache → static → virtual.
//  4. Check role hierarchy.
//  5. Store AccessContext on gin via setAccessContext.
func (a *AccessControl) Authenticate(c *gin.Context, requiredRole UserRole) bool {
	if c.Request.Context().Value(internalRequestKey) == true {
		a.setSyntheticContext(c, requiredRole, "internal-request")
		return true
	}

	cred := extractCredential(c, a.config)
	if !cred.found() {
		a.rejectAuth(c, http.StatusUnauthorized, missingCredentialDetail(c))
		return false
	}

	ac, err := a.core.Authenticate(cred.key)
	if err != nil {
		stashRejectedKeyFingerprint(c, cred.key)
		a.rejectAuth(c, http.StatusUnauthorized,
			fmt.Sprintf("Invalid %s API key (presented via %s)", requiredRole, cred.scheme))
		return false
	}

	if !control.RoleAllowed(ac.Key.Role, requiredRole) {
		c.Set(obsproxy.CtxKeyHashedAPIKey, ac.Key.Fingerprint)
		a.rejectAuth(c, http.StatusForbidden,
			fmt.Sprintf("Insufficient permissions: this endpoint requires %s access", requiredRole))
		return false
	}

	setAccessContext(c, ac)
	return true
}

// AuthenticateMultiRole accepts any of the given roles. An empty
// acceptedRoles slice rejects every request — treating "no roles
// accepted" as "no one gets in" is safer than panicking on the
// bypass path or letting the membership check silently allow.
func (a *AccessControl) AuthenticateMultiRole(c *gin.Context, acceptedRoles []UserRole) bool {
	if len(acceptedRoles) == 0 {
		a.rejectAuth(c, http.StatusForbidden,
			"Insufficient permissions: no roles accepted for this endpoint")
		return false
	}
	if c.Request.Context().Value(internalRequestKey) == true {
		a.setSyntheticContext(c, acceptedRoles[0], "internal-request")
		return true
	}

	cred := extractCredential(c, a.config)
	if !cred.found() {
		a.rejectAuth(c, http.StatusUnauthorized, missingCredentialDetail(c))
		return false
	}

	ac, err := a.core.Authenticate(cred.key)
	if err != nil {
		stashRejectedKeyFingerprint(c, cred.key)
		a.rejectAuth(c, http.StatusUnauthorized,
			fmt.Sprintf("Invalid API key (presented via %s)", cred.scheme))
		return false
	}

	if !slices.Contains(acceptedRoles, ac.Key.Role) {
		c.Set(obsproxy.CtxKeyHashedAPIKey, ac.Key.Fingerprint)
		a.rejectAuth(c, http.StatusForbidden,
			fmt.Sprintf("Insufficient permissions: this endpoint requires one of %v", acceptedRoles))
		return false
	}

	setAccessContext(c, ac)
	return true
}

// stashRejectedKeyFingerprint surfaces the truncated fingerprint of a
// rejected API key onto the gin context so the proxy-metric middleware
// can label the 401 series. Length-gated at 12: KeyFingerprint returns
// the raw input verbatim for shorter strings, which would let an
// attacker explode the metric's hashed_api_key dimension by spraying
// short junk strings as the key. Real keys generated by the keys
// package are always &gt; 12 characters, so the gate is a no-op for
// genuine traffic.
func stashRejectedKeyFingerprint(c *gin.Context, rawKey string) {
	if len(rawKey) <= 12 {
		return
	}
	c.Set(obsproxy.CtxKeyHashedAPIKey, control.KeyFingerprint(rawKey))
}

// AuthenticateRaw validates a raw API key and returns the resolved
// AccessContext. Used by OptionalAuthMiddleware which needs to attach
// the context without running the role-hierarchy check. No gin concerns.
func (a *AccessControl) AuthenticateRaw(apiKey string) (*AccessContext, error) {
	return a.core.Authenticate(apiKey)
}

// setSyntheticContext creates a synthetic AccessContext for bypass paths.
// The fingerprint argument is a human-readable descriptor of the bypass
// origin ("internal-request", "cluster-proxy"). It populates KeyPrincipal.Name
// — the alias dimension. KeyPrincipal.Fingerprint resolves to the
// "synthetic" sentinel so the proxy metric's hashed_api_key label and
// alias label remain distinct (LiteLLM convention treats them as
// independent dimensions, and operators filtering by hashed_api_key
// MUST NOT see synthetic traffic mixed in with real-key traffic).
func (a *AccessControl) setSyntheticContext(c *gin.Context, role UserRole, fingerprint string) {
	setAccessContext(c, &AccessContext{
		Key: &KeyPrincipal{
			ID:          string(role),
			Role:        role,
			Name:        fingerprint,
			Fingerprint: "synthetic",
		},
	})
}

// defaultAuthResponder answers on any path with no dialect attached,
// which is every management route. Problem Details is that surface's
// dialect, and the responder is stateless, so one instance is shared.
var defaultAuthResponder = newProblemResponder()

// rejectAuth writes an auth failure in the dialect of the surface the
// caller is on. A 401 on /v1/* has to look like an OpenAI error, not a
// Problem Details body: a client that only knows how to read one
// envelope is exactly the client an auth failure needs to reach.
func (a *AccessControl) rejectAuth(c *gin.Context, status int, detail string) {
	logAuthAttempt(c, a.config, status, detail)
	r := httperr.FromContextOr(c, defaultAuthResponder)
	switch status {
	case http.StatusUnauthorized:
		r.Unauthorized(c, detail)
	case http.StatusForbidden:
		r.Forbidden(c, detail)
	default:
		RespondWithProblem(c, status, http.StatusText(status), detail)
	}
	c.Abort()
}

// ===== Enforcement =====

// Enforce runs the full enforcement chain after authentication and once
// the model name is known. Short-circuits for anonymous / static-key
// paths (no quotas); otherwise delegates to core.Enforce and translates
// the Decision into HTTP.
//
// Order per design doc §"Adapter call flow — pinned order":
//  1. Read AccessContext from gin.
//  2. Short-circuit: anonymous → gated-guard check; static key → allow.
//  3. Call core.Enforce(ac, modelName).
//  4. Unconditionally write decision.RateLimitHeaders.
//  5. On deny → writeQuotaDenial; on allow → stash Release.
func (a *AccessControl) Enforce(c *gin.Context, modelName string) bool {
	ac, admitted := a.resolveVirtualOrRespond(c, modelName)
	if ac == nil {
		return admitted
	}

	decision, release, reservationID := a.core.Enforce(ac, modelName)

	// Write rate-limit headers unconditionally — nil map is a no-op.
	for k, v := range decision.RateLimitHeaders {
		c.Writer.Header().Set(k, v)
	}

	if !decision.Allowed {
		a.writeQuotaDenial(c, decision)
		return false
	}

	c.Set(string(CtxKeyConcurrencyRelease), release)
	if reservationID != control.NoReservation {
		c.Set(string(CtxKeyReservationID), reservationID)
	}
	return true
}

// Authorize admits a request that consumes no tokens: every check
// Enforce makes except RPM/TPM, budget and concurrency. Nothing is held,
// so there is nothing to release.
func (a *AccessControl) Authorize(c *gin.Context, modelName string) bool {
	ac, admitted := a.resolveVirtualOrRespond(c, modelName)
	if ac == nil {
		return admitted
	}
	if decision := a.core.Authorize(ac, modelName); !decision.Allowed {
		a.writeQuotaDenial(c, decision)
		return false
	}
	return true
}

// resolveVirtualOrRespond settles the callers Core never sees. A nil
// context with admitted=true is static or anonymous traffic let through;
// on admitted=false the response is already written. A non-nil context
// is a virtual key for Core to judge.
//
// Whether an anonymous caller is admitted at all is the auth
// middleware's call (anonymousRefusal); what is left here is the one
// refusal that needs the model: a cloud model bills this node's account.
func (a *AccessControl) resolveVirtualOrRespond(c *gin.Context, modelName string) (*control.AccessContext, bool) {
	ac := GetAccessContext(c)
	if ac == nil {
		if a.anonymousCloudDenied(c.Request.Context(), modelName) {
			a.rejectAuth(c, http.StatusUnauthorized,
				fmt.Sprintf("model %q is served by a cloud provider and is billed to this node's upstream account; "+
					"authenticate with an API key, or pick a locally served model", modelName))
			return nil, false
		}
		return nil, true
	}
	if ac.Key == nil || !ac.Key.IsVirtual {
		return nil, true
	}
	return ac, true
}

// MirrorReservationIDToRecorder copies the ReservationID that Enforce
// stashed on gin.Context onto the InferenceRecorder so the log bridge
// carries it through to RecordSpendByKey. Called from dispatcher paths
// that run Enforce and then attach (or have already attached) a
// recorder — see resolveAndDispatchWithRecorder. A dispatcher that
// forgets this step still works via the reaper fallback; spend simply
// does not settle for that request.
func (a *AccessControl) MirrorReservationIDToRecorder(c *gin.Context, rec *llm.InferenceRecorder) {
	if rec == nil {
		return
	}
	v, ok := c.Get(string(CtxKeyReservationID))
	if !ok {
		return
	}
	id, ok := v.(control.ReservationID)
	if !ok {
		return
	}
	rec.SetReservationID(uint64(id))
}

// writeQuotaDenial renders an HTTP error for a denied Core.Decision by
// switching on the DenialReason enum. Status, error class and
// Retry-After are derived here — Core carries no HTTP fields.
func (a *AccessControl) writeQuotaDenial(c *gin.Context, d control.Decision) {
	switch d.Reason {
	case control.DenyKeySuspended, control.DenyTeamSuspended,
		control.DenyKeyExpired, control.DenyModelAccess:
		a.rejectAuth(c, http.StatusForbidden, d.Message)
		return
	}

	status := http.StatusTooManyRequests
	errType := "rate_limit_error"
	code := "rate_limit_exceeded"
	switch d.Reason {
	case control.DenyBudget:
		status = http.StatusPaymentRequired
		errType = "insufficient_quota"
		code = "insufficient_quota"
	}

	e := httperr.Error{Status: status, Type: errType, Message: d.Message, Code: code}

	switch d.Reason {
	// Budget denials carry the machine-readable spend ledger snapshot, so
	// clients can render "X used of Y" without a follow-up usage call. A
	// budget denial without a limit has no ledger to show.
	case control.DenyBudget:
		if d.SpendLimitMicro <= 0 {
			break
		}
		// Period reset is the absolute "you can definitely retry by"
		// moment — the boundary at which spend resets to zero.
		nextReset := quota.NextPeriodStart(d.PeriodStart, quota.ParsePeriod(d.BudgetPeriod))
		retryAfterSec := quotaRetryAfterSeconds(d)
		c.Header("Retry-After", strconv.FormatInt(retryAfterSec, 10))
		e.Extra = map[string]any{"zzrouter": gin.H{
			"scope":               d.Scope,
			"entity_id":           d.EntityID,
			"spend_limit_usd":     quota.MicroToUSD(d.SpendLimitMicro),
			"spend_used_usd":      quota.MicroToUSD(d.SpendUsedMicro),
			"spend_held_usd":      quota.MicroToUSD(d.SpendHeldMicro),
			"period":              d.BudgetPeriod,
			"next_reset_at":       nextReset.UTC().Format(time.RFC3339),
			"retry_after_seconds": retryAfterSec,
		}}

	// RPM/TPM/Concurrency denials carry the throttle context so agents
	// can pick a retry strategy without parsing rate-limit headers.
	// Mirrors the 402 block (scope, entity_id, retry_after_seconds,
	// next_reset_at), with limit_kind to distinguish the trip cause.
	case control.DenyRPM, control.DenyTPM, control.DenyConcurrency:
		limitKind, retryAfterSec := throttleRetryHint(d)
		c.Header("Retry-After", strconv.FormatInt(retryAfterSec, 10))
		e.Extra = map[string]any{"zzrouter": gin.H{
			"scope":               d.Scope,
			"entity_id":           d.EntityID,
			"limit_kind":          limitKind,
			"retry_after_seconds": retryAfterSec,
			"next_reset_at":       utils.NowUTC().Add(time.Duration(retryAfterSec) * time.Second).Format(time.RFC3339),
		}}
	}

	// The request's dialect renders it: on a surface that does not speak
	// OpenAI the shape IS the contract (an Ollama client unmarshals
	// `error` as a string and fails outright on an object). The zzrouter
	// block rides beside the error; clients ignore members they do not know.
	writeError(c.Writer, c.Request, e)
	c.Abort()
}

// budgetHeldRetryHintSec is the retry hint when reservations are holding
// the budget but settled spend is still under the limit: peer requests
// settle within seconds and free most of the held estimate, so the caller
// should come back soon rather than wait for the period boundary.
const budgetHeldRetryHintSec = 5

// quotaRetryAfterSeconds is the Retry-After a denial deserves,
// independent of the envelope it is rendered in.
//
// Budget: settled spend already over the limit means the caller can only
// retry at the period boundary (possibly days out), so the hint is the
// delta to that boundary; clients may downgrade to the absolute
// next_reset_at when it is too long. Reservations holding the budget with
// actuals still under it settle in seconds — see budgetHeldRetryHintSec.
// Throttles read their own reset window. Returns 0 when the reason
// carries no useful hint.
func quotaRetryAfterSeconds(d control.Decision) int64 {
	switch d.Reason {
	case control.DenyRPM, control.DenyTPM, control.DenyConcurrency:
		_, seconds := throttleRetryHint(d)
		return seconds
	case control.DenyBudget:
		if d.SpendLimitMicro <= 0 {
			return 0
		}
		if d.SpendUsedMicro < d.SpendLimitMicro {
			return budgetHeldRetryHintSec
		}
		seconds := int64(time.Until(
			quota.NextPeriodStart(d.PeriodStart, quota.ParsePeriod(d.BudgetPeriod))).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		return seconds
	default:
		return 0
	}
}

// throttleRetryHint derives (limit_kind, retry_after_seconds) for a
// 429 quota denial. RPM/TPM read the reset duration from the rate-
// limit headers Core attached to the decision (formatted as "59s" /
// "1500ms" / "0s" — see pkg/access/quota.formatDuration). Concurrency
// has no fixed reset window; 5s mirrors the budget-not-exhausted hint.
// Returns at least 1s so clients always have a positive Retry-After.
func throttleRetryHint(d control.Decision) (string, int64) {
	switch d.Reason {
	case control.DenyRPM:
		return "rpm", parseRateLimitReset(d.RateLimitHeaders["x-ratelimit-reset-requests"])
	case control.DenyTPM:
		return "tpm", parseRateLimitReset(d.RateLimitHeaders["x-ratelimit-reset-tokens"])
	default: // DenyConcurrency
		return "concurrency", concurrencyRetryHintSec
	}
}

// concurrencyRetryHintSec is the suggested retry delay when a request
// is denied by the per-key concurrency limiter. Concurrent slots free
// as in-flight requests complete, typically within seconds; 5s avoids
// hot-spinning while still recovering quickly when one slot frees.
const concurrencyRetryHintSec = 5

// parseRateLimitReset converts the rate-limit-reset header value
// produced by pkg/access/quota.formatDuration ("59s" / "1500ms" /
// "0s") into ceil-seconds. Returns 1 on parse failure or non-positive
// values so the wire always carries a positive Retry-After.
func parseRateLimitReset(formatted string) int64 {
	if formatted == "" {
		return 1
	}
	d, err := time.ParseDuration(formatted)
	if err != nil || d <= 0 {
		return 1
	}
	secs := int64(d.Seconds())
	if d > time.Duration(secs)*time.Second {
		secs++ // round up sub-second remainder
	}
	if secs < 1 {
		secs = 1
	}
	return secs
}

// CancelPendingReservationIfUnsettled is called post-response. If Enforce
// stashed a budget reservation and the inference log bridge did NOT
// settle it (handler errored, stream never started, upstream refused,
// log bridge disabled), this pops the oldest stash and cancels it
// immediately — reclaiming budget without waiting for the 30-second
// reaper. Under normal success flow the bridge settles synchronously
// during stream completion and this is a no-op.
func (a *AccessControl) CancelPendingReservationIfUnsettled(c *gin.Context) {
	ac := GetAccessContext(c)
	if ac == nil || ac.Key == nil || !ac.Key.IsVirtual {
		return
	}
	val, ok := c.Get(string(CtxKeyReservationID))
	if !ok {
		return
	}
	id, ok := val.(control.ReservationID)
	if !ok {
		return
	}
	a.core.CancelPending(ac.Key.ID, id)
}

// ReleaseConcurrency is called by the deferred middleware after response
// is written. Reads the release func stored by Enforce and invokes it.
// No-op if no slot was acquired.
func ReleaseConcurrency(c *gin.Context) {
	val, exists := c.Get(string(CtxKeyConcurrencyRelease))
	if !exists {
		return
	}
	if release, ok := val.(func()); ok && release != nil {
		release()
	}
}

// ===== Spend / admin passthroughs =====

func (a *AccessControl) GetKeySpend(keyID string) *quota.SpendState {
	return a.core.GetKeySpend(keyID)
}

func (a *AccessControl) GetTeamSpend(teamID string) *quota.SpendState {
	return a.core.GetTeamSpend(teamID)
}

func (a *AccessControl) ResetKeySpend(keyID string) {
	a.core.ResetKeySpend(keyID)
}

func (a *AccessControl) ResetTeamSpend(teamID string) {
	a.core.ResetTeamSpend(teamID)
}

func (a *AccessControl) GetAllSpendStates() map[string]*quota.SpendState {
	return a.core.GetAllSpendStates()
}

func (a *AccessControl) RecordSpendByKey(keyID string, reservationID uint64, costMicro, tokensIn, tokensOut int64) {
	a.core.RecordSpendByKey(keyID, control.ReservationID(reservationID), costMicro, tokensIn, tokensOut)
}

func (a *AccessControl) OnKeyDeleted(keyID string) {
	a.core.OnKeyDeleted(keyID)
}

func (a *AccessControl) OnTeamDeleted(teamID string) {
	a.core.OnTeamDeleted(teamID)
}

func (a *AccessControl) InvalidateAuthCache() {
	a.core.InvalidateAuthCache()
}

// FlushSpend persists dirty spend state to disk without stopping the
// enforcer. Called by Server.Stop before HTTP drain as a pre-drain
// phase so the final flush isn't racing the shutdown deadline.
func (a *AccessControl) FlushSpend() {
	if a == nil {
		return
	}
	a.core.FlushSpend()
}

// HasGatedTeam reports whether any team restricts its keys' models.
func (a *AccessControl) HasGatedTeam() bool {
	if a == nil || a.core == nil {
		return false
	}
	return a.core.HasGatedTeam()
}

// buildStaticKeySet extracts the two static keys from node.yaml. Returns
// an empty set if any key is unconfigured (the authenticator treats
// empty values as "never matches").
func buildStaticKeySet(cfg *pkgConfig.NodeConfig) StaticKeySet {
	var set StaticKeySet
	if admin, err := cfg.Auth.GetAdminAPIKey(); err == nil {
		set.Admin = admin
	}
	if user, err := cfg.Auth.GetUserAPIKey(); err == nil {
		set.User = user
	}
	return set
}
