package prov_apps

import (
	"fmt"
	"maps"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

func (m *ProviderAppManager) launchSchema(provider string) (*schema.ProviderSchema, error) {
	if key, _, ok := m.resolveConfigKey(provider); ok {
		provider = key
	}
	if m.providerSchema == nil {
		merged, _ := schema.Merge(provider, nil, config.ServiceConfig{})
		return merged, nil
	}
	merged, err := m.providerSchema(provider)
	if err != nil {
		return nil, fmt.Errorf("provider schema: %w", err)
	}
	return merged, nil
}

func filterLaunchAuto(declared *schema.ProviderSchema, endpoint string, parameters map[string]string) map[string]string {
	parameters = maps.Clone(parameters)
	shapes := declared.ForEndpoint(endpoint)
	for key, value := range parameters {
		if dropsAuto(key, value, shapes[key]) {
			delete(parameters, key)
		}
	}
	return parameters
}

// dropsAuto reports whether a launch leaves key to the engine: auto on a key
// whose shape declares no auto value, which lets a request neutralise a
// configured one.
func dropsAuto(key, value string, shape schema.ParamShape) bool {
	return config.IsAuto(value) && !runsettings.IsParameter(key) && shape.Auto == ""
}
