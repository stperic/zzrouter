package control

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ===== Tuning constants =====

const (
	// pendingReservationTTL bounds how long a stashed reservation can
	// live in pendingReservations before the reaper cancels it. The
	// FIFO cross-settlement risk that drove the original tight value
	// is gone (each stash has its own ID, popped by ID not FIFO), so
	// this is now purely a memory leak bound for handler-error paths.
	//
	// The value must exceed realistic worst-case inference latency
	// because settlement happens post-response: when a model launches
	// cold (vLLM 7B 30-60s, llama.cpp first map ~30s, embedding cold
	// ~10s), the request can hold its reservation longer than a tight
	// TTL — the reaper would then cancel the stash before the bridge
	// settles, and the ledger (RequestCount, TokensIn/Out, SpendUSD)
	// would never reflect that request. 5min covers the common cold
	// loads with margin; truly enormous models (70B, multi-GPU) that
	// take longer should tune this via a future config knob.
	//
	// Worst-case memory leak under sustained handler-error rate R is
	// bounded by R × pendingReservationTTL stashes — small even at
	// 1000 req/s with 100% error rate.
	pendingReservationTTL = 5 * time.Minute

	// pendingReservationReapInterval is how often the reaper walks the
	// pendingReservations map. Must be shorter than the TTL; kept small
	// because the walk is cheap (single lock + range over an expected-
	// empty map under normal load).
	pendingReservationReapInterval = 5 * time.Second
)

// Core owns the pure-domain access control pipeline: authentication,
// authorization, quota enforcement, and post-response spend settlement.
// All methods are gin-free. Server wraps this in a gin adapter that
// translates HTTP requests and responses.
type Core struct {
	authenticator *Authenticator
	authorizer    *Authorizer
	enforcer      *quota.Enforcer

	keys   keys.KeyStore
	teams  teams.TeamStore
	groups modelgroup.GroupReader

	// pendingReservations tracks in-flight budget reservations by
	// (keyID, ReservationID) so the inference log bridge can settle
	// the exact reservation that belongs to a given response without
	// relying on FIFO ordering — two concurrent requests on the same
	// key used to race against each other's settle when one errored
	// out and the other settled later with the wrong cost.
	//
	// Each entry carries a creation timestamp so the reaper can expire
	// stashes whose bridge callback never fires (handler errored before
	// response started, log bridge disabled, etc.). Without the reaper
	// the map would grow unbounded.
	pendingMu           sync.Mutex
	pendingReservations map[string]map[ReservationID]pendingReservationEntry
	reservationSeq      atomic.Uint64 // monotonically assigned stash IDs; atomic.Uint64 guarantees 8-byte alignment

	pendingReaperStop chan struct{}
	pendingReaperOnce sync.Once
	// wg tracks owned goroutines (reaper today; future background workers).
	// Stop signals shutdown and blocks on wg.Wait before returning so the
	// reaper cannot race with enforcer.Stop against SpendTracker locks.
	wg sync.WaitGroup
}

// ReservationID identifies a single budget reservation batch that
// Enforce stashed for post-response settlement. Zero is reserved for
// "no reservation held" (a request without budget caps); valid IDs
// start at 1 and are assigned by a process-local atomic counter.
type ReservationID uint64

// NoReservation is the sentinel returned by Enforce when no budget
// stash was created — either the request had no spend limits or
// enforcement denied before the reservation step.
const NoReservation ReservationID = 0

// pendingReservationEntry is a single stashed reservation batch for a
// key. The timestamp is the pendingMu-serialized push time; the reaper
// uses it to decide when a stash is stale.
type pendingReservationEntry struct {
	reservations []quota.ReservationRecord
	createdAt    time.Time
}

// NewCore wires the collaborators. Caller must call Start() to begin
// background goroutines (auth cache cleanup, reservation reaper,
// rate-limiter cleanup, spend persistence).
func NewCore(
	staticKeys StaticKeySet,
	keyStore keys.KeyStore,
	teamStore teams.TeamStore,
	groups modelgroup.GroupReader,
	enforcer *quota.Enforcer,
	identity ModelIdentity,
) *Core {
	c := &Core{
		authenticator:       NewAuthenticator(staticKeys, keyStore),
		authorizer:          NewAuthorizer(groups, identity),
		enforcer:            enforcer,
		keys:                keyStore,
		teams:               teamStore,
		groups:              groups,
		pendingReservations: make(map[string]map[ReservationID]pendingReservationEntry),
		pendingReaperStop:   make(chan struct{}),
	}
	if teamStore == nil {
		slog.Info("control.Core constructed without a team store; team features disabled")
	}
	return c
}

// Start launches background goroutines: authenticator cache cleanup,
// reservation reaper, enforcer subsystems.
//
// INVARIANT: Core owns these goroutines. Callers must NOT wrap Start in
// a `go` statement — the subsystem owns its own goroutines.
func (c *Core) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.authenticator.Start()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.pendingReservationReaper()
	}()
	if c.enforcer != nil {
		c.enforcer.Start(ctx)
	}
}

// Stop terminates goroutines and flushes quota state to disk. Restart-safe.
func (c *Core) Stop(ctx context.Context) {
	if c == nil {
		return
	}
	c.authenticator.Stop()
	c.pendingReaperOnce.Do(func() {
		close(c.pendingReaperStop)
	})
	// Wait for the reaper to finish before stopping the enforcer —
	// the reaper calls into SpendTracker via enforcer.CancelReservations
	// and must not race with enforcer.Stop.
	c.wg.Wait()
	if c.enforcer != nil {
		c.enforcer.Stop(ctx)
	}
}

// Authenticate resolves a raw API key to an AccessContext (with team
// resolution attached). Pure — no HTTP concerns.
//
// Returns (nil, error) when the key is empty, invalid, expired, or
// suspended. The error message carries enough information for the
// adapter to log + return 401 with a sanitized message.
func (c *Core) Authenticate(rawKey string) (*AccessContext, error) {
	p, err := c.authenticator.Validate(rawKey)
	if err != nil {
		return nil, err
	}
	return c.buildAccessContext(p), nil
}

// buildAccessContext resolves the key's team and constructs the
// AccessContext. Every virtual key carries a TeamID at creation; the
// role comes from the key itself, not a team-side membership map.
func (c *Core) buildAccessContext(p *KeyPrincipal) *AccessContext {
	ac := &AccessContext{Key: p}

	if !p.IsVirtual || c.teams == nil || p.TeamID == "" {
		return ac
	}

	team := c.teams.Get(p.TeamID)
	if team == nil {
		return ac
	}

	ac.Team = &TeamPrincipal{
		ID:            team.ID,
		Name:          team.Name,
		MemberRole:    teams.MemberRole(p.TeamRole),
		AllowedModels: team.AllowedModels,
		Quotas:        team.QuotaConfig,
		Suspended:     team.Suspended,
		SuspendedAt:   team.SuspendedAt,
		SuspendedBy:   team.SuspendedBy,
	}
	return ac
}

// Enforce runs the full enforcement chain after authentication and once
// the model name is known. Pure — returns a Decision and a Release
// callback; HTTP rendering is adapter's responsibility.
//
// INVARIANT — enforcement order must not change:
//  1. Suspended check (key, then team)
//  2. Expired check (key)
//  3. Model access (team.AllowedModels intersection, group-aware)
//     (steps 1-3 are Authorize; Enforce runs it first)
//  4. Quota chain — key scope first, team scope second — RPM → TPM → budget reservation
//  5. Concurrency slot acquisition
//
// On concurrency failure, in-flight budget reservations are cancelled
// before returning (preserves the access_control.go:360-363 invariant).
//
// Callers pass a non-nil AccessContext with IsVirtual=true. Static keys
// and anonymous paths are short-circuited by the adapter BEFORE reaching
// Enforce — they don't have quotas.
//
// On success with a non-zero budget reservation, returns a
// ReservationID the caller MUST thread to RecordSpendByKey so the
// settlement lands on the right reservation even when multiple
// concurrent requests on the same key are in flight. When no
// reservation is held (no spend caps configured or the chain had no
// spend step), the returned ID is NoReservation.
func (c *Core) Enforce(ac *AccessContext, modelName string) (Decision, Release, ReservationID) {
	if d := c.Authorize(ac, modelName); !d.Allowed {
		return d, nil, NoReservation
	}

	// Build quota chain: key first, then team.
	chain := buildQuotaChain(ac)

	// 4. Quota chain (RPM/TPM/budget reservation).
	qd, reservations := c.enforcer.Check(chain)

	// Headers are snapshotted AFTER Check so "remaining" counts this
	// request. Taken before, a fresh key answered its first call with
	// remaining == limit, and a client self-throttling on the header
	// overshot by one every window.
	headers := c.enforcer.WriteRateLimitHeaders(chain)
	if !qd.Allowed {
		return Decision{
			Reason:           translateQuotaReason(qd.Reason),
			Message:          humanQuotaMessage(qd),
			Scope:            string(qd.Scope),
			EntityID:         qd.EntityID,
			RateLimitHeaders: headers,
			SpendLimitMicro:  qd.SpendLimitMicro,
			SpendUsedMicro:   qd.SpendUsedMicro,
			SpendHeldMicro:   qd.SpendHeldMicro,
			BudgetPeriod:     qd.BudgetPeriod,
			PeriodStart:      qd.PeriodStart,
		}, nil, NoReservation
	}

	// 5. Concurrency acquisition.
	release, concDecision := c.enforcer.AcquireConcurrency(chain)
	if !concDecision.Allowed {
		// Preserve invariant: cancel budget reservations before returning.
		c.enforcer.CancelReservations(reservations)
		return Decision{
			Reason:           DenyConcurrency,
			Message:          humanQuotaMessage(concDecision),
			Scope:            string(concDecision.Scope),
			EntityID:         concDecision.EntityID,
			RateLimitHeaders: headers,
		}, nil, NoReservation
	}

	// Stash reservations keyed by (keyID, ReservationID) so the
	// inference-log bridge settles the exact stash this request
	// created, even when multiple concurrent requests on the same
	// key are in flight.
	id := c.stashReservations(ac.Key.ID, reservations)

	return Decision{
		Allowed:          true,
		RateLimitHeaders: headers,
	}, release, id
}

// Authorize runs the admission checks that cost the caller nothing:
// suspension, expiry and model access. Enforce runs it first, then the
// metered steps. A request that consumes no tokens (counting them, for
// one) calls Authorize alone, so it neither ticks RPM nor holds budget.
func (c *Core) Authorize(ac *AccessContext, modelName string) Decision {
	// 1. Suspended check.
	if ac.Key.Suspended {
		return Decision{
			Reason:  DenyKeySuspended,
			Message: fmt.Sprintf("API key %q is suspended", ac.Key.ID),
		}
	}
	if ac.Team != nil && ac.Team.Suspended {
		return Decision{
			Reason:  DenyTeamSuspended,
			Message: fmt.Sprintf("Team %q is suspended", ac.Team.ID),
		}
	}

	// 2. Expired check.
	if ac.Key.IsExpired() {
		return Decision{
			Reason:  DenyKeyExpired,
			Message: fmt.Sprintf("API key %q has expired", ac.Key.ID),
		}
	}

	// 3. Model access — team is the sole source of allowed models. A
	// virtual key with no team is not model-gated. Static keys never
	// reach Core; the adapter admits them first.
	if ac.Team != nil && !c.authorizer.Allowed(modelName, ac.Team.AllowedModels) {
		return Decision{
			Reason: DenyModelAccess,
			Message: fmt.Sprintf("Model access denied for key %q: model %q not in team %q allowed_models",
				ac.Key.ID, modelName, ac.Team.ID),
		}
	}

	return Decision{Allowed: true}
}

// stashReservations registers the batch under a freshly-minted ID and
// returns it. Returns NoReservation when the batch is empty so the
// caller can treat "no reservation" uniformly.
func (c *Core) stashReservations(keyID string, reservations []quota.ReservationRecord) ReservationID {
	if len(reservations) == 0 {
		return NoReservation
	}
	id := ReservationID(c.reservationSeq.Add(1))
	c.pendingMu.Lock()
	keyed, ok := c.pendingReservations[keyID]
	if !ok {
		keyed = make(map[ReservationID]pendingReservationEntry)
		c.pendingReservations[keyID] = keyed
	}
	keyed[id] = pendingReservationEntry{reservations: reservations, createdAt: utils.Now()}
	c.pendingMu.Unlock()
	return id
}

// CancelPending cancels the stashed reservation with id when it
// wasn't settled by the inference log bridge. Safe no-op when the id
// is NoReservation (no stash created) or the entry has already been
// popped (settled or reaped).
func (c *Core) CancelPending(keyID string, id ReservationID) {
	if c == nil || keyID == "" || id == NoReservation {
		return
	}
	reservations := c.popPendingReservation(keyID, id)
	if len(reservations) == 0 {
		return
	}
	c.enforcer.CancelReservations(reservations)
}

// buildQuotaChain constructs the enforcement chain from an AccessContext.
// Key scope is always first, team scope (if any) is second.
func buildQuotaChain(ac *AccessContext) []quota.QuotaScope {
	chain := []quota.QuotaScope{{
		Scope:    quota.ScopeKey,
		EntityID: ac.Key.ID,
		Quotas:   ac.Key.Quotas,
	}}
	if ac.Team != nil {
		chain = append(chain, quota.QuotaScope{
			Scope:    quota.ScopeTeam,
			EntityID: ac.Team.ID,
			Quotas:   ac.Team.Quotas,
		})
	}
	return chain
}

// translateQuotaReason maps the string Reason returned by quota.Enforcer
// to the Core's typed DenialReason enum. The adapter switches on the
// enum to derive HTTP shape.
func translateQuotaReason(quotaReason string) DenialReason {
	switch quotaReason {
	case "rpm_limit_exceeded":
		return DenyRPM
	case "tpm_limit_exceeded":
		return DenyTPM
	case "budget_exhausted":
		return DenyBudget
	case "concurrency_limit_exceeded":
		return DenyConcurrency
	default:
		// Unknown reason → treat as RPM so at least a 429 is emitted.
		// If this fires, quota's vocabulary has drifted out from under
		// Core; the adapter log should catch it.
		return DenyRPM
	}
}

// humanQuotaMessage produces the operator-friendly message the adapter
// will render. Preserves the exact strings from the pre-extraction
// code at access_control.go:505-520.
func humanQuotaMessage(d quota.Decision) string {
	switch d.Reason {
	case "budget_exhausted":
		return fmt.Sprintf("Budget exhausted for %s %s", d.Scope, d.EntityID)
	case "rpm_limit_exceeded":
		return fmt.Sprintf("RPM limit exceeded for %s %s", d.Scope, d.EntityID)
	case "tpm_limit_exceeded":
		return fmt.Sprintf("TPM limit exceeded for %s %s", d.Scope, d.EntityID)
	case "concurrency_limit_exceeded":
		return fmt.Sprintf("Too many concurrent requests for %s %s", d.Scope, d.EntityID)
	default:
		return fmt.Sprintf("%s for %s %s", d.Reason, d.Scope, d.EntityID)
	}
}
