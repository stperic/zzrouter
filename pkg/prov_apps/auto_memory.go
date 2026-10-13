package prov_apps

import (
	"fmt"
	"maps"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

func (m *ProviderAppManager) filterLaunchAuto(provider string, parameters map[string]string) (map[string]string, string, error) {
	filtered := config.FilterAutoValues(parameters)
	if m.memoryBudget == nil {
		return filtered, "", nil
	}
	if key, _, ok := m.resolveConfigKey(provider); ok {
		provider = key
	}
	budget, err := m.memoryBudget(provider)
	if err != nil {
		return nil, "", err
	}
	if budget == nil {
		return filtered, "", nil
	}
	auto := budget.Kind == schema.MemoryFractionTotal && (parameters[budget.Parameter] == "auto" || parameters[strings.ReplaceAll(budget.Parameter, "-", "_")] == "auto")
	if budget.Kind != schema.MemoryReserveMiB && !auto {
		return filtered, "", nil
	}
	parameters = maps.Clone(parameters)
	for _, key := range []string{budget.Parameter, budget.DeviceCountParameter} {
		if alias := strings.ReplaceAll(key, "-", "_"); alias != key {
			if value, ok := parameters[alias]; ok {
				if canonical, present := parameters[key]; present && canonical != value {
					return nil, "", fmt.Errorf("%w: ambiguous memory parameter spellings %q and %q; use the hyphenated spelling", process.ErrMemoryBudgetInvalid, key, alias)
				}
				delete(parameters, alias)
				parameters[key] = value
			}
		}
	}
	filtered = config.FilterAutoValues(parameters)
	if !auto {
		return filtered, "", nil
	}
	filtered[budget.Parameter] = "auto"
	return filtered, budget.Parameter, nil
}
