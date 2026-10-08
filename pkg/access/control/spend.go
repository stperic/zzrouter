package control

import (
	"log/slog"

	"github.com/stperic/zzrouter/pkg/access/quota"
)

// RecordSpendByKey settles the specific reservation stash identified
// by reservationID and records token consumption. Called post-response
// by the inference log bridge. A reservationID of NoReservation means
// this request had no budget cap (or enforcement didn't reach the
// reservation step); tokens are still recorded for TPM, but nothing is
// settled against the spend ledger.
//
// If the key belongs to a team, TPM tokens are recorded on the team
// scope too.
func (c *Core) RecordSpendByKey(keyID string, reservationID ReservationID, costMicro, tokensIn, tokensOut int64) {
	if keyID == "" {
		return
	}

	// Settle the identified reservation (if any). A missing entry
	// means the reaper already cancelled it or the bridge fired twice
	// for the same request — either is safe to ignore.
	if reservationID != NoReservation {
		reservations := c.popPendingReservation(keyID, reservationID)
		if len(reservations) > 0 {
			c.enforcer.SettleReservations(reservations, costMicro, tokensIn, tokensOut)
		} else {
			// The bridge fired with a non-zero ID but the stash is gone.
			// Expected failure mode: the reaper cancelled it first (handler
			// held the reservation longer than pendingReservationTTL).
			// Tokens still flow to TPM so rate-limit math stays honest.
			slog.Warn("audit-spend settle found no stash; reaper likely cancelled first",
				"key_id", keyID,
				"reservation_id", uint64(reservationID))
		}
	}

	// Record TPM tokens for rate limiting (key scope).
	keyScope := quota.ThrottleScope{Kind: quota.ScopeKey, EntityID: keyID}
	c.enforcer.RecordTokens(keyScope, tokensIn+tokensOut)

	// Team-scope tokens: read TeamID directly off the stored key.
	if c.keys != nil {
		if vk := c.keys.Get(keyID); vk != nil && vk.TeamID != "" {
			teamScope := quota.ThrottleScope{Kind: quota.ScopeTeam, EntityID: vk.TeamID}
			c.enforcer.RecordTokens(teamScope, tokensIn+tokensOut)
		}
	}
}

// GetKeySpend returns the current-period spend state for a virtual key,
// or nil if no traffic has been recorded yet. The returned value is a
// snapshot — mutating it has no effect on the live tracker.
func (c *Core) GetKeySpend(keyID string) *quota.SpendState {
	if c.enforcer == nil || keyID == "" {
		return nil
	}
	return c.enforcer.GetSpendState(quota.SpendScope{Kind: quota.ScopeKey, EntityID: keyID})
}

// GetTeamSpend returns the current-period spend state for a team, or nil.
func (c *Core) GetTeamSpend(teamID string) *quota.SpendState {
	if c.enforcer == nil || teamID == "" {
		return nil
	}
	return c.enforcer.GetSpendState(quota.SpendScope{Kind: quota.ScopeTeam, EntityID: teamID})
}

// ResetKeySpend zeros the ledger for a key. No-op if not tracked.
func (c *Core) ResetKeySpend(keyID string) {
	if c.enforcer == nil || keyID == "" {
		return
	}
	c.enforcer.ResetSpend(quota.SpendScope{Kind: quota.ScopeKey, EntityID: keyID})
}

// ResetTeamSpend zeros the ledger for a team. No-op if not tracked.
func (c *Core) ResetTeamSpend(teamID string) {
	if c.enforcer == nil || teamID == "" {
		return
	}
	c.enforcer.ResetSpend(quota.SpendScope{Kind: quota.ScopeTeam, EntityID: teamID})
}

// GetAllSpendStates returns a snapshot of every tracked spend scope,
// keyed by SpendScope.String() ("kind:entityID:model"). Safe to consume
// while enforcement is running.
func (c *Core) GetAllSpendStates() map[string]*quota.SpendState {
	if c.enforcer == nil {
		return map[string]*quota.SpendState{}
	}
	return c.enforcer.GetAllSpendStates()
}

// OnKeyDeleted purges the key's spend ledger rows and invalidates the
// authenticator cache. Cascade-delete of a personal team is handled by
// the server-side keys service, not here.
func (c *Core) OnKeyDeleted(keyID string) {
	if c.enforcer != nil {
		c.enforcer.ForgetEntity(quota.ScopeKey, keyID)
	}
	c.authenticator.InvalidateAll()
}

// OnTeamDeleted purges a team's spend ledger. Called post-delete so
// team-scope SpendState rows don't linger.
func (c *Core) OnTeamDeleted(teamID string) {
	if c.enforcer != nil {
		c.enforcer.ForgetEntity(quota.ScopeTeam, teamID)
	}
}

// InvalidateAuthCache clears the authenticator cache. Call after key
// rotation, reload, or bulk mutations.
func (c *Core) InvalidateAuthCache() {
	c.authenticator.InvalidateAll()
}

// FlushSpend persists dirty spend state to disk without stopping the
// enforcer. Server.Stop calls this before HTTP drain so a slow shutdown
// can't push the final flush past systemd's SIGKILL deadline.
func (c *Core) FlushSpend() {
	if c.enforcer != nil {
		c.enforcer.FlushSpend()
	}
}

// HasGatedTeam exposes the underlying team store's gated-team flag for
// the anonymous-compat guard.
func (c *Core) HasGatedTeam() bool {
	if c.teams == nil {
		return false
	}
	return c.teams.HasGatedTeam()
}
