package prov_apps

import (
	"fmt"
	"maps"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
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
	if budget == nil || budget.Kind != "fraction_total" {
		return filtered, "", nil
	}
	if parameters[budget.Parameter] != "auto" && parameters[strings.ReplaceAll(budget.Parameter, "-", "_")] != "auto" {
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
	filtered[budget.Parameter] = "auto"
	return filtered, budget.Parameter, nil
}
