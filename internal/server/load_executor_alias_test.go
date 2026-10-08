package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// One model is reachable by several names — the repo it came from and
// the file it landed in — and a launch registers it under whichever one
// this node knows it by. These tests drive the load path with the OTHER
// one, which is the only name a caller who launched by repo id holds.
const (
	aliasLoadProvider = "fake"
	aliasLoadRepoID   = "nomic-ai/nomic-embed-text-v1.5-GGUF"
	aliasLoadFileStem = "nomic-embed-text-v1.5.Q4_K_M"
)

// aliasLoadConfig is a provider whose launch command never references
// ${MODEL_PATH}, so these tests need no weights on the machine running
// them. What is under test is the name the load path looks an instance
// up by, not whether the model exists.
func aliasLoadConfig(t *testing.T) *config.AppsConfig {
	t.Helper()
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp(aliasLoadProvider, config.ServiceConfig{
		Enabled:  new(true),
		Name:     "Fake",
		Protocol: config.ProtocolOpenAI,
		Mode:     "on-demand",
		Runtime: &config.AppRuntimeConfig{
			// Its own range: pkg/prov_apps' alias tests hold 8200-8205
			// and the two packages run as separate processes.
			PortRange: []int{8300, 8305},
			BasePort:  8300,
			KeepAlive: "1m",
			Execution: config.ExecutionConfig{
				Type: "cli",
				// Outlives the assertions without outliving the test.
				// A child that exits immediately is reaped and marked
				// failed, and the already-running branch below only
				// fires for a starting or running instance.
				Command: "sleep",
				Args:    []string{"2"},
			},
			HealthCheck: config.HealthcheckConfig{Path: "/health"},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	return cfg
}

// emptyModelStore is a model store holding nothing. The load path
// consults it only after the existing-instance check has missed, so a
// test whose model IS running should never reach it — and when one
// does, the 404 it produces names the fall-through precisely.
type emptyModelStore struct{}

func (emptyModelStore) ListAllModels() ([]*metadata.ModelMetadata, error) {
	return nil, nil
}

// newAliasLoadExecutor wires a LoadExecutor over a real provider manager
// that resolves aliasLoadRepoID to aliasLoadFileStem, the way the model
// cache resolves a SourceID. The returned counter records how many
// launches the executor actually asked for.
func newAliasLoadExecutor(t *testing.T) (*LoadExecutor, *prov_apps.ProviderAppManager, *int) {
	t.Helper()

	mgr, err := prov_apps.NewProviderAppManager(aliasLoadConfig(t),
		prov_apps.WithNodename(func() string { return "node1" }),
		prov_apps.WithModelCanonicalizer(func(name string) string {
			if name == aliasLoadRepoID {
				return aliasLoadFileStem
			}
			return name
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	launches := 0
	exec := NewLoadExecutor(
		mgr,
		func(string) bool { return true },
		emptyModelStore{},
		func(string) (*backend.Resolved, bool) { return nil, false },
		nil, // mergeAndResolve belongs to the preview path, not this one
		func(ctx context.Context, model, provider, endpoint string, _, _ map[string]string) (*instance.Instance, error) {
			launches++
			return mgr.LaunchInstance(ctx, prov_apps.LaunchRequest{
				Provider: provider,
				Model:    model,
				Endpoint: prov_apps.Endpoint(endpoint),
			})
		},
		func() {},                     // invalidateCache
		func() string { return "n1" }, // getNodename
		nil,                           // refreshResourceMetrics
	)
	return exec, mgr, &launches
}

// A force reload had to find the running instance before it could stop
// it, and it looked for the caller's name in a registry keyed by the
// resolved one. Nothing matched, so nothing was stopped — and then
// LaunchInstance, which does resolve the alias, handed back the very
// process the caller asked to replace. The route answered 200 and
// restarted nothing.
func TestLoadLocalModel_ForceRestartsARunNamedByItsRepoID(t *testing.T) {
	exec, mgr, launches := newAliasLoadExecutor(t)

	existing, err := mgr.LaunchInstance(context.Background(), prov_apps.LaunchRequest{
		Provider: aliasLoadProvider,
		Model:    aliasLoadRepoID,
	})
	require.NoError(t, err)
	require.Equal(t, aliasLoadFileStem, existing.Model,
		"the launch should have registered the run under the resolved name")

	resp, err := exec.LoadLocalModel(context.Background(), &LoadModelRequest{
		ModelName: aliasLoadRepoID,
		Provider:  aliasLoadProvider,
		Force:     true,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	if _, stillRegistered := mgr.GetInstance(existing.ID); stillRegistered {
		t.Error("force left the original instance in place")
	}
	assert.Equal(t, 1, *launches, "force did not launch a replacement")
	assert.NotEqual(t, existing.ID, resp.InstanceID,
		"force answered with the instance it was asked to replace")
}

// The same mismatch on the read side. A model already up, asked for by
// the repo id it was launched with, fell past the existing-instance
// check and on into the model store — which matches on file name, not
// repo id, so a running model was answered "has not been downloaded".
func TestLoadLocalModel_ReportsARunningRunNamedByItsRepoID(t *testing.T) {
	exec, mgr, launches := newAliasLoadExecutor(t)

	existing, err := mgr.LaunchInstance(context.Background(), prov_apps.LaunchRequest{
		Provider: aliasLoadProvider,
		Model:    aliasLoadFileStem,
	})
	require.NoError(t, err)

	resp, err := exec.LoadLocalModel(context.Background(), &LoadModelRequest{
		ModelName: aliasLoadRepoID,
		Provider:  aliasLoadProvider,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, http.StatusConflict, resp.StatusCode,
		"a running model was not reported as already running")
	assert.Equal(t, existing.ID, resp.InstanceID)
	assert.Equal(t, 0, *launches, "a running model was launched a second time")
}
