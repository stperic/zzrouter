package cache

import (
	"maps"
	"slices"
	"strings"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// expandVariants lists each provider-config variant wherever its base's
// weights are: one entry per node holding the base, assigned to the
// variant's provider and naming its base in VariantOf. That makes a
// variant a model like any other to everything that reads the catalog:
// /v1/models lists it, the coordinator routes it to a node holding the
// weights, a worker finds its provider.
//
// A name the catalog already holds on a node keeps that entry, so a
// variant never shadows real weights. Admission rejects conflicting names
// until the operator disambiguates them. Variants of an explicitly disabled
// provider are not listed, since nothing would launch them.
func expandVariants(models []*CachedModel, cfg *pkgConfig.AppsConfig) []*CachedModel {
	if cfg == nil {
		return nil
	}
	// Names compare case-insensitively, as the config's model keys do.
	type placed struct{ name, node string }
	have := make(map[placed]bool, len(models))
	for _, m := range models {
		have[placed{strings.ToLower(m.Name), m.Node}] = true
	}
	var out []*CachedModel
	cfg.RangeApps(func(app string, sc pkgConfig.ServiceConfig) bool {
		if sc.Mode != constants.AppModeOnDemand || sc.IsExplicitlyDisabled() {
			return true
		}
		variants := sc.Variants()
		for _, name := range slices.Sorted(maps.Keys(variants)) {
			base := variants[name]
			for _, m := range models {
				// The base is named as the catalog names the weights, not by
				// an alias: the walk resolves its cells under that name.
				if m.IsCloud || !strings.EqualFold(m.Name, base) {
					continue
				}
				if m.Provider != "" && m.Provider != app {
					continue // the weights belong to another engine
				}
				at := placed{strings.ToLower(name), m.Node}
				if have[at] {
					continue
				}
				have[at] = true
				v := *m
				v.Name, v.Model, v.SourceID, v.VariantOf, v.Provider = name, name, "", base, app
				out = append(out, &v)
			}
		}
		return true
	})
	return out
}
