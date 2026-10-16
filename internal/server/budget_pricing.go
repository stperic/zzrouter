package server

import (
	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// quoteTargets removes unknown prices only when a spend limit needs them.
func (s *Server) quoteTargets(c *gin.Context, target *resolver.Resolved) {
	var store *pricing.Store
	if s.model != nil {
		store = s.model.Pricing
	}
	quote := func(provider, model string) pricing.Quote {
		local := false
		if s.appsConfig != nil {
			if svc, ok := s.appsConfig.LookupApp(provider); ok {
				local = prov_apps.Lifecycle(svc) != prov_apps.LifecycleNone
			}
		}
		return pricing.Resolve(store, provider, []string{model}, local)
	}
	limited := control.HasSpendLimit(GetAccessContext(c))
	if len(target.Candidates) == 0 {
		target.Quotes = []pricing.Quote{quote(target.Provider, target.ModelName)}
	} else {
		kept := target.Candidates[:0:0]
		for _, cand := range target.Candidates {
			q := quote(cand.App, cand.Model)
			if limited && q.Rates == nil {
				store.RecordMiss(q.Provider, q.Model)
				continue
			}
			kept = append(kept, cand)
			target.Quotes = append(target.Quotes, q)
		}
		target.Candidates = kept
		if len(kept) > 0 {
			target.ModelName, target.Provider, target.Node = kept[0].Model, kept[0].App, kept[0].Node
		} else {
			target.Quotes = []pricing.Quote{{Provider: target.Provider, Model: target.ModelName}}
		}
		return
	}
	if limited && target.Quotes[0].Rates == nil {
		store.RecordMiss(target.Provider, target.ModelName)
	}
}
