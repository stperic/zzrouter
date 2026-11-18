package control

import (
	"fmt"
	"slices"

	"github.com/stperic/zzrouter/pkg/access/teams"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
)

// ModelIdentity reduces a model reference to the identity access control
// compares on.
//
// The catalog publishes decorated ids (`qwen2.5:0.5b@macbook-pro`) while
// the dispatcher enforces on the bare name it strips out of them, so an
// allow list populated from the catalog would match nothing. Both sides
// go through this function, which makes the two id spaces one. Parsing
// lives with the caller because `@` means something else in a cloud id
// and only the server knows which registries are cloud.
//
// The zero value is meaningful: a nil ModelIdentity compares references
// verbatim.
type ModelIdentity func(model string) string

func (f ModelIdentity) of(model string) string {
	if f == nil {
		return model
	}
	return f(model)
}

// Authorizer checks whether a team is allowed to use a given model.
// Every virtual key inherits its access policy from its team's
// AllowedModels. An empty list is a wildcard. Group names expand to
// their deployment models via the GroupReader.
type Authorizer struct {
	groups   modelgroup.GroupReader
	identity ModelIdentity
}

// NewAuthorizer constructs an Authorizer with the supplied GroupReader
// and model-identity normalizer. A nil groups reader disables group-name
// expansion; literal model names still match. A nil identity compares
// references verbatim.
func NewAuthorizer(groups modelgroup.GroupReader, identity ModelIdentity) *Authorizer {
	return &Authorizer{groups: groups, identity: identity}
}

// Allowed reports whether the requested model is in the team's allow list.
// A nil/empty teamModels list is a wildcard and always returns true. Group
// names in the list are expanded via groups.Get(name).Replicas.
func (a *Authorizer) Allowed(modelName string, teamModels []string) bool {
	if len(teamModels) == 0 {
		return true // wildcard
	}

	want := a.identity.of(modelName)

	for _, entry := range teamModels {
		if a.identity.of(entry) == want {
			return true
		}

		// Group name expansion — check if the requested model is a
		// replica in this group.
		if a.groups != nil {
			if group := a.groups.Get(entry); group != nil {
				for _, rep := range group.Replicas {
					if a.identity.of(rep.Model) == want {
						return true
					}
				}
			}
		}
	}

	return false
}

// ProviderAllowed applies team policy to a resolved provider identity.
func ProviderAllowed(ac *AccessContext, provider string) bool {
	return ac == nil || ac.Key == nil || !ac.Key.TeamBound() || ac.Team != nil && ac.Team.ProviderAccess.Allows(provider)
}

// RouteAllowed requires an exact route identity that still exists.
func (a *Authorizer) RouteAllowed(name string, policy teams.RouteAccess) bool {
	if !policy.Allows(name) || a.groups == nil {
		return false
	}
	g := a.groups.Get(name)
	return g != nil && g.Available()
}

// RoutesRestricted reports whether a virtual caller must use assigned routes.
func RoutesRestricted(ac *AccessContext) bool {
	return ac != nil && ac.Key != nil && ac.Key.TeamBound() && (ac.Team == nil || ac.Team.RouteAccess.Restricted())
}

// DestinationAllowed applies route-only model ceilings and provider policy.
// Direct mode keeps authorization on the requested alias, before resolution.
func (c *Core) DestinationAllowed(ac *AccessContext, model, provider string) bool {
	return ProviderAllowed(ac, provider) && (!RoutesRestricted(ac) || c.authorizer.Allowed(model, ac.Team.AllowedModels))
}

// RouteResolved prevents a pin or missing route from becoming a direct request.
func RouteResolved(ac *AccessContext, requested, resolved string) bool {
	if ac != nil && ac.Key != nil && ac.Key.TeamBound() && ac.Team == nil {
		return false
	}
	return !RoutesRestricted(ac) || requested != "" && requested == resolved && ac.Team.RouteAccess.Allows(resolved)
}

// ValidateRoutes checks assignments against the current route catalog.
func (c *Core) ValidateRoutes(policy teams.RouteAccess) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	for _, name := range policy.Routes {
		if !c.authorizer.RouteAllowed(name, policy) {
			return fmt.Errorf("route %q is not available", name)
		}
	}
	return nil
}

// TeamContext projects current team policy for an administrator's dry run.
func (c *Core) TeamContext(id string) *AccessContext {
	if snapshot(c.state).Team(id) == nil {
		return nil
	}
	ac, _ := c.buildAccessContext(&KeyPrincipal{ID: "preview", IsVirtual: true, TeamID: id, Role: RoleUser})
	return ac
}

// RouteTeams returns the sorted explicit assignments, without a reverse store.
func (c *Core) RouteTeams(name string) []string {
	ids := []string{}
	for id, team := range snapshot(c.state).Teams() {
		if team.RouteAccess.Mode == "selected" && team.RouteAccess.Allows(name) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}
