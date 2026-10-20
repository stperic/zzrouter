package prov_apps

import (
	"errors"
	"maps"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/schema"

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
	instResult, err := m.EnsureRun(t.Context(), EnsureRequest{Provider: "vllm", Model: "llama3", Parameters: params})
	inst := instResult.Run
	require.NoError(t, err)
	return inst
}

// A config change a restart would pick up makes the run stale, naming
// each key it changes; until then the run is current.
func TestParametersStatus_ConfigChangeIsStale(t *testing.T) {
	m := tierManager(t, "")
	inst := launchLlama(t, m, nil)

	assert.Equal(t, &instance.ParametersStatus{Provider: "vllm", State: instance.ParametersCurrent}, m.ParametersStatus(t.Context(), inst),
		"judged against the config key")

	reloadWith(t, m, func(svc *config.ServiceConfig) {
		svc.Models["llama3"] = config.ModelSpec{
			Parameters:  map[string]string{"ctx-size": "changed", "seed": "1"},
			Environment: map[string]string{"ZZ_ENV": "changed"},
		}
	})
	got := m.ParametersStatus(t.Context(), inst)
	assert.Equal(t, instance.ParametersStale, got.State)
	assert.Equal(t, []string{"environment.ZZ_ENV", "parameters.ctx-size", "parameters.seed"}, got.Changed)
}

func TestParametersStatusDataDirChangeAndRestartPreparation(t *testing.T) {
	r := newContractRig(t, false)
	svc, ok := r.m.AppsConfig().LookupApp("generic")
	require.True(t, ok)
	svc.RunScope, svc.Protocol, svc.Capabilities = config.RunScopeNode, "", nil
	svc.Runtime.DataDir = "data"
	svc.Runtime.Execution.Args = []string{config.PlaceholderDataDir}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", svc))
	r.m.ReloadConfig(cfg)
	req := EnsureRequest{Provider: "generic"}
	prepared, err := processEngine{r.m}.prepare(t.Context(), svc, "generic", req, 8740)
	require.NoError(t, err)
	run, err := r.m.newRun(req, svc, 8740)
	require.NoError(t, err)
	defer run.Cancel()
	run.SetResolved(prepared.resolved)
	require.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(t.Context(), run).State)

	next := &config.AppsConfig{}
	svc.Runtime.DataDir = "storage"
	require.NoError(t, next.AddApp("generic", svc))
	r.m.ReloadConfig(next)
	status := r.m.ParametersStatus(t.Context(), run)
	require.Equal(t, instance.ParametersStale, status.State)
	require.Contains(t, status.Changed, "runtime.execution")
	restarted, err := processEngine{r.m}.prepare(t.Context(), svc, "generic", restartRequest(run), 8740)
	require.NoError(t, err)
	require.NotEqual(t, prepared.resolved.Execution, restarted.resolved.Execution)
	run.SetResolved(restarted.resolved)
	require.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(t.Context(), run).State)
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
	got := m.ParametersStatus(t.Context(), inst)
	assert.Equal(t, instance.ParametersCurrent, got.State)
	assert.Empty(t, got.Changed)
	assert.Equal(t, []string{"parameters.ctx-size"}, got.Overridden)
}

// A spelling is not a change: a run asked for ctx_size over a configured
// only_default is current, and its request key holds the configured flag.
func TestParametersStatus_SpellingIsNotAChange(t *testing.T) {
	m := tierManager(t, "")
	reloadWith(t, m, func(svc *config.ServiceConfig) { svc.Defaults.Parameters = map[string]string{"only_default": "d"} })
	inst := launchLlama(t, m, map[string]string{"ctx_size": "mine"})
	require.Equal(t, map[string]string{"ctx-size": "mine", "only-default": "d"}, inst.Resolved().Parameters)

	got := m.ParametersStatus(t.Context(), inst)
	assert.Equal(t, instance.ParametersCurrent, got.State)
	assert.Empty(t, got.Changed)
	assert.Equal(t, []string{"parameters.ctx-size"}, got.Overridden)
}

// A file a parameter names is read by the engine, so new content under
// the same name is a change, though no parameter value moved.
func TestParametersStatus_FileContentChangeIsStale(t *testing.T) {
	digest := "one"
	m, _ := localizerManager(t, func(_, _, _ string, params map[string]string, _ map[string]schema.ParamShape) (Localized, error) {
		out := maps.Clone(params)
		out["template"] = "/node/t.jinja"
		return Localized{Params: out, Files: map[string]string{"template": digest}}, nil
	})
	inst := launchLlama(t, m, map[string]string{"template": "t.jinja"})
	require.Equal(t, instance.ParametersCurrent, m.ParametersStatus(t.Context(), inst).State)

	digest = "two"
	got := m.ParametersStatus(t.Context(), inst)
	assert.Equal(t, instance.ParametersStale, got.State)
	assert.Equal(t, []string{"parameters.template"}, got.Changed)
}

// When the config as it stands would refuse a relaunch, there is nothing
// to compare, and restarting would leave the model down: unknown, why.
func TestParametersStatus_UnlaunchableConfigIsUnknown(t *testing.T) {
	refuse := false
	m, _ := localizerManager(t, func(_, _, _ string, params map[string]string, _ map[string]schema.ParamShape) (Localized, error) {
		if refuse {
			return Localized{}, errors.New("asset gone")
		}
		return Localized{Params: params}, nil
	})
	inst := launchLlama(t, m, nil)

	refuse = true
	got := m.ParametersStatus(t.Context(), inst)
	assert.Equal(t, instance.ParametersUnknown, got.State)
	assert.Contains(t, got.Error, "asset gone")
}

// Run listings carry the status for live runs only: a stopped run has
// nothing left to bring up to date.
func TestInstanceInfo_StatusOnlyForLiveRuns(t *testing.T) {
	m := tierManager(t, "")
	inst := launchLlama(t, m, nil)

	inst.SetStatus(instance.StatusRunning)
	require.NotNil(t, m.InstanceInfo(t.Context(), inst, "").ParametersStatus)

	inst.SetStatus(instance.StatusStopped)
	assert.Nil(t, m.InstanceInfo(t.Context(), inst, "").ParametersStatus)

	inst.SetStatus(instance.StatusRunning)
	assert.Nil(t, m.ListInstances("")[0].ParametersStatus, "a plain listing does not re-resolve every run")
}

func TestParametersStatus_RunSettings(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "tree", true: "request"}[explicit], func(t *testing.T) {
			m := tierManager(t, "")
			var params map[string]string
			if explicit {
				params = map[string]string{config.KeepAliveParameter: "10m"}
			}
			inst := launchLlama(t, m, params)
			reloadWith(t, m, func(svc *config.ServiceConfig) {
				svc.Models["llama3"].Parameters[config.KeepAliveParameter] = "3m"
			})
			got := m.ParametersStatus(t.Context(), inst)
			if explicit {
				assert.Equal(t, instance.ParametersCurrent, got.State)
				assert.Contains(t, got.Overridden, "parameters.keep-alive")
				assert.Equal(t, params, restartRequest(inst).Parameters)
			} else {
				assert.Equal(t, instance.ParametersStale, got.State)
				assert.Contains(t, got.Changed, "parameters.keep-alive")
			}
		})
	}
}

func TestParametersStatus_ServedEvictionSetting(t *testing.T) {
	r := newServedRig(t)
	r.edit(t, func(svc *config.ServiceConfig) {
		svc.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{config.EvictIdleParameter: "true"}}
	})
	run := r.running(t, EnsureRequest{Provider: "ollama", Model: servedModel})
	r.edit(t, func(svc *config.ServiceConfig) { svc.Defaults.Parameters[config.EvictIdleParameter] = "false" })
	assert.Contains(t, r.m.ParametersStatus(t.Context(), run).Changed, "parameters.evict-idle-on-memory-pressure")
}

func TestOperatorSchemaParametersStatus(t *testing.T) {
	var manager *ProviderAppManager
	r := newContractRig(t, false, WithProviderSchema(func(provider string) (*schema.ProviderSchema, error) {
		svc, _ := manager.AppsConfig().LookupApp(provider)
		merged, errs := schema.Merge(provider, &schema.YAMLSchema{Parameters: map[string]schema.YAMLParam{"budget": {Type: "string"}}}, svc)
		return merged, errors.Join(errs...)
	}))
	manager = r.m
	edit := func(declaration config.ParameterDeclaration) {
		svc, _ := manager.AppsConfig().LookupApp("generic")
		svc.Schema = map[string]config.ParameterDeclaration{"future": declaration}
		svc.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{"budget": "0.55", "future": "auto"}}
		next := &config.AppsConfig{}
		require.NoError(t, next.AddApp("generic", svc))
		manager.ReloadConfig(next)
	}
	edit(config.ParameterDeclaration{Type: new("string")})
	started := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "m"})})[0]
	require.NoError(t, started.err)
	run := started.result.Run
	awaitRunning(t, run)
	require.Equal(t, instance.ParametersCurrent, manager.ParametersStatus(t.Context(), run).State)
	edit(config.ParameterDeclaration{Type: new("string"), Description: new("metadata only")})
	assert.Equal(t, instance.ParametersCurrent, manager.ParametersStatus(t.Context(), run).State)
	edit(config.ParameterDeclaration{Type: new("string"), Auto: new("auto")})
	status := manager.ParametersStatus(t.Context(), run)
	assert.Equal(t, instance.ParametersStale, status.State)
	assert.Contains(t, status.Changed, "parameters.future")
}
