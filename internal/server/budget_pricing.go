package server

import (
	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/fallback"
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
	quote := s.targetQuote
	limited := control.HasSpendLimit(GetAccessContext(c))
	if len(target.Candidates) == 0 {
		target.Quotes = []pricing.Quote{quote(target.Provider, target.ModelName)}
	} else {
		kept, quotes, skipped := pricedCandidates(target.Candidates, limited, quote)
		target.Quotes = quotes
		for _, candidate := range skipped {
			store.RecordMiss(candidate.App, candidate.Model)
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

func (s *Server) targetQuote(provider, model string) pricing.Quote {
	var store *pricing.Store
	if s.model != nil {
		store = s.model.Pricing
	}

	local := false
	if apps := s.appsConfig(); apps != nil {
		if svc, ok := apps.LookupApp(provider); ok {
			local = prov_apps.Lifecycle(svc) != prov_apps.LifecycleNone
		}
	}
	return pricing.Resolve(store, provider, []string{model}, local)
}

func pricedCandidates(candidates []fallback.Candidate, limited bool, quote func(string, string) pricing.Quote) ([]fallback.Candidate, []pricing.Quote, []fallback.Candidate) {
	kept := make([]fallback.Candidate, 0, len(candidates))
	quotes := make([]pricing.Quote, 0, len(candidates))
	var skipped []fallback.Candidate
	for _, candidate := range candidates {
		q := quote(candidate.App, candidate.Model)
		if limited && q.Rates == nil {
			skipped = append(skipped, candidate)
			continue
		}
		kept, quotes = append(kept, candidate), append(quotes, q)
	}
	return kept, quotes, skipped
}
