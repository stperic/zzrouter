package prov_apps

import (
	"time"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// Activity reports observed work without catalog, process or parameter probes.
func (m *ProviderAppManager) Activity(now time.Time) (instance.Activity, map[string]instance.Activity) {
	byProvider := make(map[string][]instance.Activity)
	all := make([]instance.Activity, 0)
	for _, run := range m.instances.List() {
		key, _, ok := m.resolveConfigKey(run.Provider)
		if !ok {
			key = run.Provider
		}
		value := run.ActivitySnapshot(now)
		byProvider[key] = append(byProvider[key], value)
		all = append(all, value)
	}
	providers := make(map[string]instance.Activity, len(byProvider))
	for key, values := range byProvider {
		providers[key] = instance.SummarizeActivity(values, now)
	}
	return instance.SummarizeActivity(all, now), providers
}
