package control

import "time"

// DenialReason is the enumerated reason Core rejected an enforce call.
// Adapter code switches on this value to derive HTTP status codes, OpenAI
// error types/codes, and Retry-After headers. The pure package is
// deliberately free of HTTP-shaped fields to keep it reusable for
// non-HTTP surfaces (e.g. future gRPC or cluster-internal dispatch).
type DenialReason int

const (
	// DenyNone is the zero value — means Allowed=true; never set on denials.
	DenyNone DenialReason = iota
	// DenyKeySuspended — the virtual key itself is suspended.
	DenyKeySuspended
	// DenyTeamSuspended — the key's team is suspended (key itself is fine).
	DenyTeamSuspended
	// DenyKeyExpired — the virtual key's ExpiresAt is in the past.
	DenyKeyExpired
	// DenyModelAccess — requested model is not in the team's AllowedModels.
	DenyModelAccess
	// DenyRPM — requests-per-minute quota exhausted.
	DenyRPM
	// DenyTPM — tokens-per-minute quota exhausted.
	DenyTPM
	// DenyBudget — spend budget reservation exhausted.
	DenyBudget
	// DenyConcurrency — concurrent-request slot cap exceeded.
	DenyConcurrency
	// DenyAnonymousGated — anonymous request to a gated /v1/* or /api/*
	// path when at least one team has AllowedModels configured.
	DenyAnonymousGated
)

// String returns a stable lowercase identifier suitable for logs. The
// adapter switches on the enum value (not the string) to derive HTTP
// shape — String is purely for observability.
func (r DenialReason) String() string {
	switch r {
	case DenyNone:
		return "none"
	case DenyKeySuspended:
		return "key_suspended"
	case DenyTeamSuspended:
		return "team_suspended"
	case DenyKeyExpired:
		return "key_expired"
	case DenyModelAccess:
		return "model_access_denied"
	case DenyRPM:
		return "rpm_limit_exceeded"
	case DenyTPM:
		return "tpm_limit_exceeded"
	case DenyBudget:
		return "budget_exhausted"
	case DenyConcurrency:
		return "concurrency_limit_exceeded"
	case DenyAnonymousGated:
		return "anonymous_gated"
	default:
		return "unknown"
	}
}

// Decision is the pure-domain outcome of Core.Enforce. It is intentionally
// free of HTTP-shaped fields (status codes, OpenAI error types, Retry-After
// values) — those are adapter concerns derived by switching on Reason.
// RateLimitHeaders stays because it IS pure domain data (values emitted by
// the rate limiter), not HTTP semantics.
type Decision struct {
	Allowed bool
	Reason  DenialReason
	Message string // sanitized, human-readable

	// Scope + EntityID identify the binding scope for a quota denial.
	// "" for non-quota denials.
	Scope    string
	EntityID string

	// RateLimitHeaders is populated by the rate limiter once the quota
	// chain is built (Enforce step 4). It is nil/empty for step-1/2/3
	// denials (suspended/expired/model) and for the anonymous-gated
	// short-circuit. The adapter writes unconditionally — a nil map
	// write is a no-op.
	RateLimitHeaders map[string]string

	// Budget — set on Reason == DenyBudget; zero otherwise. Microdollar
	// units; the adapter converts to USD float for the wire envelope.
	SpendLimitMicro int64
	SpendUsedMicro  int64
	SpendHeldMicro  int64
	BudgetPeriod    string
	PeriodStart     time.Time // start of the current budget period (UTC)
}

// Release is the callback returned by Enforce on success; it frees the
// concurrency slot acquired in step 5. The adapter stores it on the gin
// context and the deferred middleware calls it after the response is
// written. nil when no slot was acquired (static keys, short-circuits).
//
// Defined as a type alias (not a named type) so callers can assert
// `val.(func())` on the value they stored. A named type would require
// `val.(control.Release)` at every read site.
type Release = func()
