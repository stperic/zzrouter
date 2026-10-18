package prov_apps

import (
	"context"
	"maps"
	"slices"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
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
func (m *ProviderAppManager) InstanceInfo(ctx context.Context, inst *instance.Instance, hostname string) instance.InstanceInfo {
	info := inst.ToInfo(hostname)
	if !info.Status.IsTerminal() {
		info.ParametersStatus = m.ParametersStatus(ctx, inst)
	}
	return info
}

// ParametersStatus compares what inst was launched with to the launch a
// restart would make now, from the config as it stands on this node.
func (m *ProviderAppManager) ParametersStatus(ctx context.Context, inst *instance.Instance) *instance.ParametersStatus {
	req := restartRequest(inst)
	configKey, svcCfg, ok := m.resolveConfigKey(req.Provider)
	if !ok {
		return &instance.ParametersStatus{State: instance.ParametersUnknown, Error: ErrProviderNotFound.Error()}
	}
	if _, err := m.AdmitLocalModel(ctx, req.Model); err != nil {
		return &instance.ParametersStatus{Provider: configKey, State: instance.ParametersUnknown, Error: err.Error()}
	}
	_, endpoint := runEndpoint(svcCfg, req.Endpoint)
	next, err := m.engine(Lifecycle(svcCfg)).prepare(ctx, svcCfg, configKey, req, inst.SnapshotConfig().Port)
	if err != nil {
		return &instance.ParametersStatus{Provider: configKey, State: instance.ParametersUnknown, Error: err.Error()}
	}

	launched := inst.Resolved()
	changed := changedKeys(parametersPath, launched.Parameters, next.resolved.Parameters)
	changed = append(changed, changedKeys(environmentPath, launched.Environment, next.resolved.Environment)...)
	for _, key := range changedKeys(parametersPath, launched.Files, next.resolved.Files) {
		if !slices.Contains(changed, key) {
			changed = append(changed, key)
		}
	}
	if launched.Execution != next.resolved.Execution || launched.WorkingDir != next.resolved.WorkingDir {
		changed = append(changed, "runtime.execution")
	}
	slices.Sort(changed)

	// Raw tiers still name held keys when config-only preparation is invalid.
	raw, fromConfigEnv := m.resolveRawLaunchParams(svcCfg, EnsureRequest{Provider: req.Provider, Model: req.Model}, endpoint)
	fromConfig := config.FlattenParameters(raw)
	declared := next.parameters.declared
	if declared == nil {
		declared, err = m.launchSchema(configKey)
	}
	if err == nil {
		if configured, err := m.modelLaunchParameters(ctx, svcCfg, process.ResolveModel(svcCfg, req.Model), EnsureRequest{Provider: req.Provider, Model: req.Model}, endpoint, declared); err == nil {
			fromConfig, fromConfigEnv = configured.Params, configured.Environment
		}
	}
	explicit := config.FlattenParameters(requestParameters(req.Parameters))
	freezeFeaturePaths(svcCfg, explicit)
	overridden := heldKeys(parametersPath, explicit, fromConfig)
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
