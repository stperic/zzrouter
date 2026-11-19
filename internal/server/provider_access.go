package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/constants"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/resolver"
)

// admitAccessTargets checks route identity and destination ceilings before
// endpoint preference, pricing and fallback can select a target.
func (s *Server) admitAccessTargets(c *gin.Context, target *resolver.Resolved) bool {
	ac := GetAccessContext(c)
	if !control.RouteResolved(ac, target.OriginalName, target.GroupName) {
		s.access.rejectAuth(c, http.StatusForbidden, "Use an assigned route without a node or provider pin")
		return false
	}
	if len(target.Candidates) == 0 {
		if s.access.core.DestinationAllowed(ac, target.ModelName, target.Provider) {
			return true
		}
	} else {
		kept := target.Candidates[:0:0]
		for _, candidate := range target.Candidates {
			if s.access.core.DestinationAllowed(ac, candidate.Model, candidate.App) {
				kept = append(kept, candidate)
			}
		}
		if len(kept) > 0 {
			target.Candidates = kept
			target.ModelName, target.Provider, target.Node = kept[0].Model, kept[0].App, kept[0].Node
			return true
		}
	}
	s.access.rejectAuth(c, http.StatusForbidden, "No destination satisfies the team's provider and model permissions")
	return false
}

// catalogAccess is shared by compatibility discovery, including group replicas.
// A nil predicate keeps trusted internal readers independent of caller policy.
type catalogAccess func(*gin.Context, string, string, ...string) bool

func (allow catalogAccess) permits(c *gin.Context, model, provider string, route ...string) bool {
	return allow == nil || allow(c, model, provider, route...)
}

func (allow catalogAccess) replicas(c *gin.Context, name string, replicas []modelgroup.Replica) []modelgroup.Replica {
	kept := make([]modelgroup.Replica, 0, len(replicas))
	for _, replica := range replicas {
		if allow == nil || allow(c, replica.Model, replica.App, name) {
			kept = append(kept, replica)
		}
	}
	return kept
}

func (s *Server) catalogAllowed(c *gin.Context, model, provider string, route ...string) bool {
	ac := GetAccessContext(c)
	if control.RoutesRestricted(ac) && len(route) == 0 {
		return false
	}
	requested := model
	if len(route) > 0 {
		requested = route[0]
	}
	if ac != nil && ac.Key != nil && ac.Key.TeamBound() && !s.access.core.Authorize(ac, requested).Allowed {
		return false
	}
	return s.access.core.DestinationAllowed(ac, model, provider)
}

func ollamaProviderAccessError(c *gin.Context) error {
	if control.ProviderAllowed(GetAccessContext(c), constants.AppOllama) {
		return nil
	}
	return newProblemError(http.StatusForbidden, "Forbidden", "The team's provider policy does not allow this destination")
}

func ollamaAccessError(c *gin.Context) error {
	if err := ollamaProviderAccessError(c); err != nil {
		return err
	}
	if control.RoutesRestricted(GetAccessContext(c)) {
		return newProblemError(http.StatusForbidden, "Forbidden", "Use an assigned route without a node or provider pin")
	}
	return nil
}

// ollamaMutationAccessError preserves provider policy and the canonical admin role rule.
func ollamaMutationAccessError(c *gin.Context) error {
	if err := ollamaAccessError(c); err != nil {
		return err
	}
	ac := GetAccessContext(c)
	if ac == nil || ac.Key == nil {
		return newProblemError(http.StatusUnauthorized, "Unauthorized", "Ollama mutations require an administrator credential")
	}
	if !control.RoleAllowed(ac.Key.Role, control.RoleAdmin) {
		return newProblemError(http.StatusForbidden, "Forbidden", "Ollama mutations require admin access")
	}
	return nil
}

func providerSelector(c *gin.Context) func(string) bool {
	ac := GetAccessContext(c)
	return func(provider string) bool { return control.ProviderAllowed(ac, provider) }
}
