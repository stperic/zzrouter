package server

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/dispatch/affinity"
)

func recordResponseForTest(t *testing.T, s *Server, id, provider, model string, ac *AccessContext) {
	t.Helper()
	origin := affinity.Origin{ProviderKey: provider, Model: s.modelIdentity(model), ClientModel: model}
	if group := s.model.Groups.Get(model); group != nil {
		origin.Route = model
		for _, replica := range group.Replicas {
			if replica.App == provider {
				origin.Model = replica.Model
				break
			}
		}
	}
	if backend, ok := s.backend.Resolve(provider); ok {
		origin.ConnectionRevision = backend.Revision
	}
	c, _ := gin.CreateTestContext(nil)
	setAccessContext(c, ac)
	s.inference.affinity.Record(id, responseOwner(c), origin)
}

func adminResponseOwner() *AccessContext {
	return &AccessContext{Key: &KeyPrincipal{ID: "admin", Role: RoleAdmin}}
}
