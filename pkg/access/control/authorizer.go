package control

import (
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
