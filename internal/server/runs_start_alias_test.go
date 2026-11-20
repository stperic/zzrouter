package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// One model is reachable by several names — the repo it came from and
// the file it landed in, and a launch registers it under whichever one
// this node knows it by. These tests drive the start path with the OTHER
// one, which is the only name a caller who launched by repo id holds.
const (
	aliasLoadProvider = "fake"
	aliasLoadRepoID   = "nomic-ai/nomic-embed-text-v1.5-GGUF"
	aliasLoadFileStem = "nomic-embed-text-v1.5.Q4_K_M"
)

// aliasLoadConfig is a provider whose launch command never references
// ${MODEL_PATH}, so these tests need no weights on the machine running
// them. What is under test is the name the start path looks an instance
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

// newAliasRunsExecutor wires a RunsExecutor over a real provider manager
// that resolves aliasLoadRepoID to aliasLoadFileStem, the way the model
// cache resolves a SourceID. The returned counter records how many
// launches the executor actually asked for.
func newAliasRunsExecutor(t *testing.T) (*RunsExecutor, *prov_apps.ProviderAppManager, *int) {
	t.Helper()

	launches := 0
	mgr, err := prov_apps.NewProviderAppManager(aliasLoadConfig(t),
		prov_apps.WithModelSource(func(context.Context, string) string { launches++; return "" }),
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

	exec := NewRunsExecutor(mgr, func() string { return "n1" }, mgr.AppsConfig, nil, nil, nil)
	return exec, mgr, &launches
}

// A force reload had to find the running instance before it could stop
// it, and it looked for the caller's name in a registry keyed by the
// resolved one. Nothing matched, so nothing was stopped — and then
// EnsureRun, which does resolve the alias, handed back the very
// process the caller asked to replace. The route answered 200 and
// restarted nothing.
func TestStartRunReplaceRestartsARunNamedByItsRepoID(t *testing.T) {
	exec, mgr, launches := newAliasRunsExecutor(t)

	existingResult, err := mgr.EnsureRun(context.Background(), prov_apps.EnsureRequest{
		Provider: aliasLoadProvider,
		Model:    aliasLoadRepoID,
	})
	existing := existingResult.Run
	require.NoError(t, err)
	require.Equal(t, aliasLoadFileStem, existing.Model,
		"the launch should have registered the run under the resolved name")

	resp, err := startForTest(t, t.Context(), exec, LaunchRunRequest{
		Model:    aliasLoadRepoID,
		Provider: aliasLoadProvider,
		Replace:  true,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	if _, stillRegistered := mgr.GetInstance(existing.ID); stillRegistered {
		t.Error("force left the original instance in place")
	}
	assert.Equal(t, 2, *launches, "force did not launch a replacement")
	assert.NotEqual(t, existing.ID, resp.ID,
		"force answered with the instance it was asked to replace")
}

// The same mismatch on the read side. A model already up, asked for by
// the repo id it was launched with, fell past the existing-instance
// check and on into the model store — which matches on file name, not
// repo id, so a running model was answered "has not been downloaded".
func TestStartRunReusesARunNamedByItsRepoID(t *testing.T) {
	exec, mgr, launches := newAliasRunsExecutor(t)

	existingResult, err := mgr.EnsureRun(context.Background(), prov_apps.EnsureRequest{
		Provider: aliasLoadProvider,
		Model:    aliasLoadFileStem,
	})
	existing := existingResult.Run
	require.NoError(t, err)

	resp, err := startForTest(t, t.Context(), exec, LaunchRunRequest{
		Model:    aliasLoadRepoID,
		Provider: aliasLoadProvider,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, existing.ID, resp.ID)
	assert.Equal(t, 1, *launches, "a running model was launched a second time")
}

func TestStartRunRefusesAnotherProvidersRun(t *testing.T) {
	exec, mgr, launches := newAliasRunsExecutor(t)
	existing := instance.NewInstance("other-run", "different-provider", aliasLoadFileStem, 8300, 0, 0)
	existing.MarkRunning(false, nil)
	require.NoError(t, mgr.Instances().Register(existing))
	t.Cleanup(func() { _ = mgr.Instances().Remove(existing.ID) })
	resp, err := startForTest(t, t.Context(), exec, LaunchRunRequest{Model: aliasLoadRepoID, Provider: aliasLoadProvider})
	require.Equal(t, http.StatusConflict, answeredStatus(err))
	require.ErrorContains(t, err, "held by provider different-provider")
	require.Nil(t, resp)
	require.Equal(t, instance.StatusRunning, existing.GetStatus())
	require.Zero(t, *launches)
}

func startForTest(t *testing.T, ctx context.Context, executor *RunsExecutor, req LaunchRunRequest) (*LaunchRunResponse, error) {
	t.Helper()
	req.LaunchMode = "native"
	body, err := json.Marshal(req)
	require.NoError(t, err)
	c, w := postJSON(t, string(body))
	c.Request = c.Request.WithContext(ctx)
	executor.HandleInternalLaunchRun(c) //nolint:contextcheck // Auto-deploy owns a detached job; the start inherits ctx.
	if w.Code >= http.StatusBadRequest {
		return nil, parseRoutedError(w.Code, w.Body.Bytes(), w.Header())
	}
	var response LaunchRunResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	return &response, nil
}
