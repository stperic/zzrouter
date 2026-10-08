package prov_apps

import (
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// reloadWith swaps the manager's config for the tier provider with edit
// applied, as a PATCH reaching this node does.
func reloadWith(t *testing.T, m *ProviderAppManager, edit func(*config.ServiceConfig)) {
	t.Helper()
	svc := tierServiceConfig()
	edit(&svc)
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m.ReloadConfig(cfg)
}

func launchLlama(t *testing.T, m *ProviderAppManager, params map[string]string) *instance.Instance {
	t.Helper()
	inst, err := m.LaunchInstance(t.Context(), LaunchRequest{Provider: "vllm", Model: "llama3", Parameters: params})
	require.NoError(t, err)
	return inst
}

// A config change a restart would pick up makes the run stale, naming
// each key it changes; until then the run is current.
func TestParametersStatus_ConfigChangeIsStale(t *testing.T) {
	m := tierManager(t, "")
	inst := launchLlama(t, m, nil)

	assert.Equal(t, &instance.ParametersStatus{Provider: "vllm", State: instance.ParametersCurrent}, m.ParametersStatus(inst),
		"judged against the config key")

	reloadWith(t, m, func(svc *config.ServiceConfig) {
		svc.Models["llama3"] = config.ModelSpec{
			Parameters:  map[string]string{"ctx-size": "changed", "added": "1"},
			Environment: map[string]string{"ZZ_ENV": "changed"},
		}
	})
	got := m.ParametersStatus(inst)
	assert.Equal(t, instance.ParametersStale, got.State)
	assert.Equal(t, []string{"environment.ZZ_ENV", "parameters.added", "parameters.ctx-size"}, got.Changed)
}

// A key the run was launched with explicitly is replayed by a restart, so
// changing it in config does not make the run stale; it is reported as
// overridden instead, which is why the change does not reach the run.
func TestParametersStatus_RequestTierHoldsItsKeys(t *testing.T) {
	m := tierManager(t, "")
	inst := launchLlama(t, m, map[string]string{"ctx-size": "mine"})

	reloadWith(t, m, func(svc *config.ServiceConfig) {
		svc.Models["llama3"] = config.ModelSpec{
			Parameters:  map[string]string{"ctx-size": "changed"},
			Environment: map[string]string{"ZZ_ENV": "tier1"},
		}
	})
	got := m.ParametersStatus(inst)
	assert.Equal(t, instance.ParametersCurrent, got.State)
	assert.Empty(t, got.Changed)
	assert.Equal(t, []string{"parameters.ctx-size"}, got.Overridden)
}

// A file a parameter names is read by the engine, so new content under
// the same name is a change, though no parameter value moved.
func TestParametersStatus_FileContentChangeIsStale(t *testing.T) {
	digest := "one"
	m, _ := localizerManager(t, func(_, _, _ string, params map[string]string) (Localized, error) {
		out := maps.Clone(params)
		out["template"] = "/node/t.jinja"
		return Localized{Params: out, Files: map[string]string{"template": digest}}, nil
	})
	inst := launchLlama(t, m, map[string]string{"template": "t.jinja"})
	require.Equal(t, instance.ParametersCurrent, m.ParametersStatus(inst).State)

	digest = "two"
	got := m.ParametersStatus(inst)
	assert.Equal(t, instance.ParametersStale, got.State)
	assert.Equal(t, []string{"parameters.template"}, got.Changed)
}

// When the config as it stands would refuse a relaunch, there is nothing
// to compare, and restarting would leave the model down: unknown, why.
func TestParametersStatus_UnlaunchableConfigIsUnknown(t *testing.T) {
	refuse := false
	m, _ := localizerManager(t, func(_, _, _ string, params map[string]string) (Localized, error) {
		if refuse {
			return Localized{}, errors.New("asset gone")
		}
		return Localized{Params: params}, nil
	})
	inst := launchLlama(t, m, nil)

	refuse = true
	got := m.ParametersStatus(inst)
	assert.Equal(t, instance.ParametersUnknown, got.State)
	assert.Contains(t, got.Error, "asset gone")
}

// Run listings carry the status for live runs only: a stopped run has
// nothing left to bring up to date.
func TestInstanceInfo_StatusOnlyForLiveRuns(t *testing.T) {
	m := tierManager(t, "")
	inst := launchLlama(t, m, nil)

	inst.SetStatus(instance.StatusRunning)
	require.NotNil(t, m.InstanceInfo(inst, "").ParametersStatus)

	inst.SetStatus(instance.StatusStopped)
	assert.Nil(t, m.InstanceInfo(inst, "").ParametersStatus)

	inst.SetStatus(instance.StatusRunning)
	assert.Nil(t, m.ListInstances("")[0].ParametersStatus, "a plain listing does not re-resolve every run")
}
