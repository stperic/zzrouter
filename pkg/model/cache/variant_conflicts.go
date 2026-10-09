package cache

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"unicode"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/utils"
)

// WeightsNamed finds physical weights by any supported catalog identity.
// Inspect every node rather than the alias index, which can shadow entries.
func (mc *Cache) WeightsNamed(ctx context.Context, name string) (string, bool, error) {
	models, err := mc.ListModels(ctx, "", "", "", "")
	if err != nil {
		return "", false, err
	}
	for _, m := range models {
		if physicalModelNamed(m, name) {
			return m.Node, true, nil
		}
	}
	return "", false, nil
}

func physicalModelNamed(m *CachedModel, name string) bool {
	name, _, _ = strings.Cut(name, "#")
	return !m.IsCloud && m.VariantOf == "" &&
		(strings.EqualFold(m.Name, name) || strings.EqualFold(utils.FormatModelName(m.Name), name) ||
			(m.SourceID != "" && strings.EqualFold(m.SourceID, name)))
}

// variantNameKey preserves EqualFold matching, including non-ASCII names.
func variantNameKey(name string) string {
	name, _, _ = strings.Cut(name, "#")
	return strings.Map(func(r rune) rune {
		smallest := r
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded < smallest {
				smallest = folded
			}
		}
		return smallest
	}, name)
}

// buildModelRefusals indexes the names admission refuses: a name whose every copy lacks its weights,
// and, overriding that, a configured variant name that real weights carry.
func buildModelRefusals(models []*CachedModel, cfg *config.AppsConfig) map[string]error {
	refusals := make(map[string]error)
	servable := make(map[string]bool)
	for _, m := range models {
		for _, key := range admissionKeys(m) {
			if !m.Incomplete {
				servable[key] = true
			} else if refusals[key] == nil {
				refusals[key] = fmt.Errorf("%w: %s (re-download to repair)", layout.ErrIncomplete, m.Name)
			}
		}
	}
	for key := range servable {
		delete(refusals, key)
	}
	variants := configuredVariants(cfg)
	for _, m := range models {
		if m.IsCloud || m.VariantOf != "" {
			continue
		}
		keys := admissionKeys(m)
		for _, key := range keys {
			if name, reserved := variants[key]; reserved {
				conflict := &config.ModelNameConflictError{Model: name, Node: m.Node}
				for _, k := range keys {
					refusals[k] = conflict
				}
				break
			}
		}
	}
	return refusals
}

// admissionKeys are the normalized names a request can use to reach m.
func admissionKeys(m *CachedModel) []string {
	aliases := []string{m.Name, utils.FormatModelName(m.Name), m.SourceID}
	if strings.EqualFold(m.Format, "gguf") && !strings.EqualFold(path.Ext(m.Name), ".gguf") {
		aliases = append(aliases, m.Name+".gguf", utils.FormatModelName(m.Name)+".gguf")
	}
	keys := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if alias != "" {
			keys = append(keys, variantNameKey(alias))
		}
	}
	return keys
}

// configuredVariants maps each on-demand variant's normalized name to its configured spelling.
func configuredVariants(cfg *config.AppsConfig) map[string]string {
	names := make(map[string]string)
	if cfg == nil {
		return names
	}
	cfg.RangeApps(func(_ string, sc config.ServiceConfig) bool {
		if sc.Mode == constants.AppModeOnDemand {
			for name := range sc.Variants() {
				names[variantNameKey(name)] = name
			}
		}
		return true
	})
	return names
}

type variantAdmissionKey struct{}
type variantAdmission struct {
	mu      sync.Mutex
	results map[*Cache]map[string]error
}

// WithVariantAdmission shares one admission result per model within a request.
// Derived contexts retain the memo across local dispatch and fallback attempts.
func WithVariantAdmission(ctx context.Context) context.Context {
	if _, ok := ctx.Value(variantAdmissionKey{}).(*variantAdmission); ok {
		return ctx
	}
	return context.WithValue(ctx, variantAdmissionKey{}, &variantAdmission{results: make(map[*Cache]map[string]error)})
}

// CheckModelAdmission reads published refusals for incomplete weights and reserved names.
func (mc *Cache) CheckModelAdmission(ctx context.Context, model string) error {
	key := variantNameKey(model)
	if memo, ok := ctx.Value(variantAdmissionKey{}).(*variantAdmission); ok {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		if err, exists := memo.results[mc][key]; exists {
			return err
		}
		err, confirmed := mc.checkModelAdmission(key)
		if !confirmed {
			return err
		}
		if memo.results[mc] == nil {
			memo.results[mc] = make(map[string]error)
		}
		memo.results[mc][key] = err
		return err
	}
	err, _ := mc.checkModelAdmission(key)
	return err
}

func (mc *Cache) checkModelAdmission(key string) (error, bool) {
	mc.mu.RLock()
	refusal, valid := mc.modelRefusals[key], mc.valid
	mc.mu.RUnlock()
	if refusal != nil {
		return refusal, true
	}
	if !valid {
		mc.mu.Lock()
		warn := !mc.valid && !mc.admissionUnavailableLogged
		if warn {
			mc.admissionUnavailableLogged = true
		}
		mc.mu.Unlock()
		if warn {
			slog.Warn("Model admission catalog unavailable; continuing without file evidence")
		}
	}
	return nil, valid
}
