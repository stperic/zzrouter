package server

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/audit"
)

// resolveActor turns a callerKeyID() result into a canonical audit actor
// string. An empty id means the request authenticated via the static
// admin key (no AccessContext is set in that path); emit it as the
// distinguishing sentinel so a compliance reviewer can tell static-key
// calls apart from virtual-key calls.
func resolveActor(callerKeyID string) string {
	if callerKeyID == "" {
		return audit.ActorStaticAdmin
	}
	return callerKeyID
}

// auditActor is the caller's principal as an audit actor, audit.ActorUnknown when
// anonymous. Controllers call it so services receive a resolved actor.
func auditActor(c *gin.Context) string {
	if principal := PrincipalFromContext(c); principal != "" {
		return principal
	}
	return audit.ActorUnknown
}

func auditContext(c *gin.Context) context.Context {
	return audit.WithIdentity(c.Request.Context(), auditActor(c), c.GetString("request_id"))
}

// recordReset records a usage reset after the reset, whose state lives outside
// the access store: best effort, so a failure is logged, not returned.
func recordReset(ctx context.Context, store access.Store, ev audit.Event) {
	if err := store.Record(ctx, ev); err != nil {
		slog.WarnContext(ctx, "Audit record failed", "action", ev.Action, "subject", ev.Subject, "error", err)
	}
}

// updateKeyChangedFields returns the fields a caller actually set on an
// UpdateKeyRequest. Keys that map to sensitive state (Suspended, spend
// caps) are always named — an auditor needs to know which knob moved.
// Never includes secret material.
func updateKeyChangedFields(req *UpdateKeyRequest) map[string]any {
	if req == nil {
		return nil
	}
	out := map[string]any{}
	if req.Name != nil {
		out["name_changed"] = true
	}
	if req.Role != nil {
		out["role"] = *req.Role
	}
	if req.Suspended != nil {
		out["suspended"] = *req.Suspended
	}
	if req.ExpiresAt != nil {
		out["expires_at"] = *req.ExpiresAt
	}
	if req.MaxParallelRequests != nil {
		out["max_parallel_requests"] = *req.MaxParallelRequests
	}
	if req.RPMLimit != nil {
		out["rpm_limit"] = *req.RPMLimit
	}
	if req.TPMLimit != nil {
		out["tpm_limit"] = *req.TPMLimit
	}
	if req.SpendLimit != nil {
		out["spend_limit"] = *req.SpendLimit
	}
	if req.ResetPeriod != nil {
		out["reset_period"] = *req.ResetPeriod
	}
	if req.Metadata != nil {
		out["metadata_changed"] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// updateTeamChangedFields mirrors updateKeyChangedFields for teams.
func updateTeamChangedFields(req *UpdateTeamRequest) map[string]any {
	if req == nil {
		return nil
	}
	out := map[string]any{}
	if req.Name != nil {
		out["name_changed"] = true
	}
	if !req.RouteAccess.IsZero() {
		out["route_access"] = req.RouteAccess.Clone()
	}
	if req.ProviderAccess.Mode != "" {
		out["provider_access"] = req.ProviderAccess.Clone()
	}
	if req.AllowedModels != nil {
		out["allowed_models_count"] = len(*req.AllowedModels)
	}
	if req.Suspended != nil {
		out["suspended"] = *req.Suspended
	}
	if req.MaxParallelRequests != nil {
		out["max_parallel_requests"] = *req.MaxParallelRequests
	}
	if req.RPMLimit != nil {
		out["rpm_limit"] = *req.RPMLimit
	}
	if req.TPMLimit != nil {
		out["tpm_limit"] = *req.TPMLimit
	}
	if req.SpendLimit != nil {
		out["spend_limit"] = *req.SpendLimit
	}
	if req.ResetPeriod != nil {
		out["reset_period"] = *req.ResetPeriod
	}
	if req.DefaultMaxTokens != nil {
		out["default_max_tokens"] = *req.DefaultMaxTokens
	}
	if len(req.Metadata) > 0 {
		out["metadata_changed"] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
