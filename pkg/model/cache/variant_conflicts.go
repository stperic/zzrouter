package cache

import (
	"context"
	"log/slog"
	"path"
	"strings"
	"sync"
	"unicode"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
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

func buildVariantConflicts(models []*CachedModel, cfg *config.AppsConfig) map[string]*config.ModelNameConflictError {
	conflicts := make(map[string]*config.ModelNameConflictError)
	if cfg == nil {
		return conflicts
	}
	names := make(map[string]string)
	cfg.RangeApps(func(_ string, sc config.ServiceConfig) bool {
		if sc.Mode == constants.AppModeOnDemand {
			for name := range sc.Variants() {
				names[variantNameKey(name)] = name
			}
		}
		return true
	})
	for _, m := range models {
		if m.IsCloud || m.VariantOf != "" {
			continue
		}
		aliases := []string{m.Name, utils.FormatModelName(m.Name), m.SourceID}
		if strings.EqualFold(m.Format, "gguf") && !strings.EqualFold(path.Ext(m.Name), ".gguf") {
			aliases = append(aliases, m.Name+".gguf", utils.FormatModelName(m.Name)+".gguf")
		}
		for _, alias := range aliases {
			name, exists := names[variantNameKey(alias)]
			if !exists {
				continue
			}
			conflict := &config.ModelNameConflictError{Model: name, Node: m.Node}
			for _, identity := range aliases {
				if identity != "" {
					conflicts[variantNameKey(identity)] = conflict
				}
			}
		}
	}
	return conflicts
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

// CheckVariantConflict consults the published conflict index without catalog or
// filesystem I/O. Catalog mutation events rebuild it; cold launches also check
// unpublished local weights before starting an engine.
func (mc *Cache) CheckVariantConflict(ctx context.Context, model string) error {
	key := variantNameKey(model)
	if memo, ok := ctx.Value(variantAdmissionKey{}).(*variantAdmission); ok {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		if err, exists := memo.results[mc][key]; exists {
			return err
		}
		err, confirmed := mc.checkVariantConflict(key)
		if !confirmed {
			return err
		}
		if memo.results[mc] == nil {
			memo.results[mc] = make(map[string]error)
		}
		memo.results[mc][key] = err
		return err
	}
	err, _ := mc.checkVariantConflict(key)
	return err
}

func (mc *Cache) checkVariantConflict(key string) (error, bool) {
	mc.mu.RLock()
	conflict, valid := mc.variantConflicts[key], mc.valid
	mc.mu.RUnlock()
	if conflict != nil {
		return conflict, true
	}
	if !valid {
		mc.mu.Lock()
		warn := !mc.valid && !mc.admissionUnavailableLogged
		if warn {
			mc.admissionUnavailableLogged = true
		}
		mc.mu.Unlock()
		if warn {
			slog.Warn("Variant admission catalog unavailable; continuing without conflict evidence")
		}
	}
	return nil, valid
}
