package prov_apps

import (
	"context"
	"maps"
	"slices"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// Path prefixes ParametersStatus names keys under, matching the config
// tree's own sections.
const (
	parametersPath  = "parameters."
	environmentPath = "environment."
)

// InstanceInfo is inst's API snapshot with its ParametersStatus. Working
// that out re-resolves the run's launch, reading its asset files, so it is
// for a caller that asked; a stopped or failed run has nothing left to
// bring up to date.
func (m *ProviderAppManager) InstanceInfo(inst *instance.Instance, hostname string) instance.InstanceInfo {
	info := inst.ToInfo(hostname)
	if !info.Status.IsTerminal() {
		info.ParametersStatus = m.ParametersStatus(inst)
	}
	return info
}

// ParametersStatus compares what inst was launched with to the launch a
// restart would make now, from the config as it stands on this node.
func (m *ProviderAppManager) ParametersStatus(inst *instance.Instance) *instance.ParametersStatus {
	req := restartRequest(inst)
	configKey, svcCfg, ok := m.resolveConfigKey(req.Provider)
	if !ok {
		return &instance.ParametersStatus{State: instance.ParametersUnknown, Error: ErrProviderNotFound.Error()}
	}
	if _, err := m.AdmitLocalModel(context.Background(), req.Model); err != nil {
		return &instance.ParametersStatus{Provider: configKey, State: instance.ParametersUnknown, Error: err.Error()}
	}
	endpoint := string(EndpointOrDefault(req.Endpoint))
	next, err := m.prepareLaunch(svcCfg, configKey, endpoint, req, inst.SnapshotConfig().Port)
	if err != nil {
		return &instance.ParametersStatus{Provider: configKey, State: instance.ParametersUnknown, Error: err.Error()}
	}

	launched := inst.Resolved()
	if key := launched.AutoMemory; key != "" && next.resolved.AutoMemory == key {
		next.resolved.Parameters[key] = launched.Parameters[key]
	}
	changed := changedKeys(parametersPath, launched.Parameters, next.resolved.Parameters)
	changed = append(changed, changedKeys(environmentPath, launched.Environment, next.resolved.Environment)...)
	for _, key := range changedKeys(parametersPath, launched.Files, next.resolved.Files) {
		if !slices.Contains(changed, key) {
			changed = append(changed, key)
		}
	}
	if launched.Execution != next.resolved.Execution {
		changed = append(changed, "runtime.execution")
	}
	slices.Sort(changed)

	// What config alone resolves to, to name the keys the run's own
	// request holds against it.
	fromConfig, fromConfigEnv := m.resolveLaunchParams(svcCfg, LaunchRequest{Provider: req.Provider, Model: req.Model}, endpoint)
	if configured, err := m.modelLaunchParameters(svcCfg, LaunchRequest{Provider: req.Provider, Model: req.Model}, endpoint); err == nil {
		fromConfig, fromConfigEnv = configured.Params, configured.Environment
	}
	overridden := heldKeys(parametersPath, req.Parameters, fromConfig)
	overridden = append(overridden, heldKeys(environmentPath, req.EnvVars, fromConfigEnv)...)

	status := &instance.ParametersStatus{Provider: configKey, State: instance.ParametersCurrent, Changed: changed, Overridden: overridden}
	if len(changed) > 0 {
		status.State = instance.ParametersStale
	}
	return status
}

// changedKeys lists, sorted and prefixed, every key whose value differs
// between before and after, including keys only one of them has.
func changedKeys(prefix string, before, after map[string]string) []string {
	var keys []string
	for _, k := range slices.Sorted(maps.Keys(before)) {
		if v, ok := after[k]; !ok || v != before[k] {
			keys = append(keys, prefix+k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(after)) {
		if _, ok := before[k]; !ok {
			keys = append(keys, prefix+k)
		}
	}
	return keys
}

// heldKeys lists, sorted and prefixed, the keys of explicit whose config
// value differs from the one explicit holds.
func heldKeys(prefix string, explicit, config map[string]string) []string {
	var keys []string
	for _, k := range slices.Sorted(maps.Keys(explicit)) {
		if v, ok := config[k]; ok && v != explicit[k] {
			keys = append(keys, prefix+k)
		}
	}
	return keys
}
