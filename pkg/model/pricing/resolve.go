package pricing

// Quote distinguishes unknown rates from an explicit free price.
type Quote struct {
	Provider, Model string
	Rates           *ModelPricing
	Local           bool // lifecycle evidence survives until settlement
}

// Resolve preserves operator precedence, then local-free evidence, then table rates.
func Resolve(store *Store, provider string, models []string, local bool, admitted ...Quote) Quote {
	q := Quote{Provider: provider, Local: local}
	for _, model := range models {
		if model == "" {
			continue
		}
		if q.Model == "" {
			q.Model = model
		}
		if store != nil {
			if mp, ok := store.LookupOverride(provider, model); ok {
				q.Model, q.Rates = model, &mp
				return q
			}
		}
	}
	if local {
		if len(admitted) > 0 && admitted[0].Rates != nil {
			return admitted[0]
		}
		q.Rates = &ModelPricing{}
		return q
	}
	if store != nil {
		for _, model := range models {
			if model == "" {
				continue
			}
			var mp ModelPricing
			var ok bool
			if provider != "" {
				mp, ok = store.LookupByProvider(provider, model)
			} else {
				mp, ok = store.Lookup(model)
			}
			if ok && mp.HasPricing() {
				q.Model, q.Rates = model, &mp
				return q
			}
		}
	}
	if len(admitted) > 0 && admitted[0].Rates != nil {
		return admitted[0]
	}
	return q
}
