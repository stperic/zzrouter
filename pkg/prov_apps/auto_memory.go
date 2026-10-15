package prov_apps

import (
	"fmt"
	"maps"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

func (m *ProviderAppManager) launchSchema(provider string) (*schema.ProviderSchema, error) {
	if key, _, ok := m.resolveConfigKey(provider); ok {
		provider = key
	}
	if m.providerSchema == nil {
		merged, _ := schema.Merge(provider, nil)
		return merged, nil
	}
	merged, err := m.providerSchema(provider)
	if err != nil {
		return nil, fmt.Errorf("provider schema: %w", err)
	}
	return merged, nil
}

func filterLaunchAuto(declared *schema.ProviderSchema, endpoint string, parameters map[string]string) (map[string]string, error) {
	var budget *schema.MemoryBudget
	if declared.Diagnostics != nil {
		budget = declared.Diagnostics.Memory
	}
	parameters = maps.Clone(parameters)
	if err := normalizeMemoryParameters(budget, parameters); err != nil {
		return nil, err
	}
	shapes := declared.ForEndpoint(endpoint)
	for key, value := range parameters {
		if !config.IsAuto(value) || runsettings.IsParameter(key) {
			continue
		}
		shape := shapes[key]
		if shape.Auto == "" {
			delete(parameters, key)
		}
	}
	return parameters, nil
}

func normalizeMemoryParameters(budget *schema.MemoryBudget, parameters map[string]string) error {
	if budget == nil {
		return nil
	}
	auto := budget.Kind == schema.MemoryFractionTotal && (config.IsAuto(parameters[budget.Parameter]) || config.IsAuto(parameters[strings.ReplaceAll(budget.Parameter, "-", "_")]))
	if !auto && budget.Kind != schema.MemoryReserveMiB {
		return nil
	}
	for _, key := range []string{budget.Parameter, budget.DeviceCountParameter} {
		alias := strings.ReplaceAll(key, "-", "_")
		if alias == key {
			continue
		}
		if value, ok := parameters[alias]; ok {
			if canonical, present := parameters[key]; present && canonical != value {
				return fmt.Errorf("%w: ambiguous memory parameter spellings %q and %q; use the hyphenated spelling", process.ErrMemoryBudgetInvalid, key, alias)
			}
			delete(parameters, alias)
			parameters[key] = value
		}
	}
	return nil
}
