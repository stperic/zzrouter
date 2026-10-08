package server

import (
	"github.com/gin-gonic/gin"

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

// auditActor is the controller-side helper: extract the calling key's ID
// from gin and convert to an audit-actor string. Controllers call this
// directly so the service layer receives an already-resolved actor.
func auditActor(c *gin.Context) string {
	return resolveActor(callerKeyID(c))
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
