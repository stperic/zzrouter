package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type preflightInstaller struct {
	install.ProviderInstaller
	calls        int
	requirements *config.AppRequirements
	report       *preflight.Report
}

func (i *preflightInstaller) ProviderName() string { return "worker-only" }

func (i *preflightInstaller) SupportedPlatforms() []fsroot.Platform { return nil }

func (i *preflightInstaller) Preflight(_ context.Context, reqs *config.AppRequirements) *preflight.Report {
	i.calls++
	i.requirements = reqs
	if reqs != nil && reqs.Python != nil {
		i.report.Results[1].Message = fmt.Sprintf("Python %s missing on localai", reqs.Python.Min)
	}
	return i.report
}

func preflightWorker(t *testing.T) (*featureResolvedRouter, *preflightInstaller, *config.AppRequirements) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	store, err := config.NewAppsConfigStore(root)
	require.NoError(t, err)
	reqs := &config.AppRequirements{Python: &config.PythonRequirement{Min: "99.1"}, DiskSpace: "987TB"}
	require.NoError(t, store.Config().UpdateApp("vllm", func(sc *config.ServiceConfig) error {
		sc.Requirements = reqs
		return nil
	}))
	mgr, err := prov_apps.NewProviderAppManager(store.Config())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		require.NoError(t, mgr.Stop(ctx))
	})
	inst := &preflightInstaller{report: &preflight.Report{
		Provider: "worker-only", AllOK: false,
		Results: []preflight.Result{
			{Check: "worker disk", Passed: true, Message: "worker storage writable"},
			{Check: "worker python", Message: "Python 99.1 missing on localai", Hint: "Choose a compatible target interpreter"},
		},
	}}
	mgr.Install().Dispatcher().Register("vllm", inst)
	executor := &ProvidersExecutor{mgr: mgr, nodeName: "localai"}
	worker := gin.New()
	worker.POST("/zzrouter/v1/internal/providers/:name/install/preflight", executor.HandleInternalPreflightInstall)
	return &featureResolvedRouter{worker: worker}, inst, reqs
}

func requestPreflight(ctx context.Context, router *featureResolvedRouter, provider, query string) *httptest.ResponseRecorder {
	ctrl := &ProvidersController{providersService: NewProvidersService(router)}
	engine := gin.New()
	engine.POST("/zzrouter/v1/providers/:name/install/preflight", ctrl.PreflightInstall)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodPost, "/zzrouter/v1/providers/"+provider+"/install/preflight"+query, nil))
	return response
}

func assertPreflightRoute(t *testing.T, router *featureResolvedRouter, provider, node string) {
	t.Helper()
	require.Len(t, router.calls, 1)
	assert.Equal(t, http.MethodPost, router.calls[0].Method)
	assert.Equal(t, "/zzrouter/v1/internal/providers/"+provider+"/install/preflight", router.calls[0].Path)
	assert.Equal(t, node, router.calls[0].Node)
	assert.Empty(t, router.calls[0].Body)
}

func TestPreflightRoutesWithoutLocalInstaller(t *testing.T) {
	for _, tc := range []struct{ query, node string }{{"?node=localai", "localai"}, {"", "localhost"}} {
		t.Run(tc.node, func(t *testing.T) {
			router, inst, _ := preflightWorker(t)
			response := requestPreflight(t.Context(), router, "vllm", tc.query)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assertPreflightRoute(t, router, "vllm", tc.node)
			assert.Equal(t, 1, inst.calls)
			assert.Contains(t, response.Body.String(), `"provider":"worker-only"`)
		})
	}
}

func TestPreflightUsesTargetRequirements(t *testing.T) {
	router, inst, reqs := preflightWorker(t)
	response := requestPreflight(t.Context(), router, "vllm", "?node=localai")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, 1, inst.calls)
	assert.Equal(t, reqs, inst.requirements)
	assert.Contains(t, response.Body.String(), "Python 99.1 missing on localai")
}

func TestPreflightPreservesFailedReport(t *testing.T) {
	router, inst, _ := preflightWorker(t)
	response := requestPreflight(t.Context(), router, "vllm", "?node=localai")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var envelope struct{ Data preflight.Report }
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	assert.Equal(t, *inst.report, envelope.Data)
}

func TestPreflightRoutingFailures(t *testing.T) {
	for _, tc := range []struct {
		name, provider, body, code string
		status                     int
		err                        error
		cancel                     bool
	}{
		{name: "target lookup", provider: "unknown", status: 404, code: "not_found"},
		{name: "transport", provider: "vllm", status: 502, code: "node_unreachable", err: errors.New("transport unavailable")},
		{name: "malformed", provider: "vllm", status: 502, code: "upstream_shape", body: `{"results":`},
		{name: "cancellation", provider: "vllm", status: 502, code: "upstream_canceled", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, inst, _ := preflightWorker(t)
			router.err = tc.err
			if tc.body != "" {
				router.worker = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.body)) })
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			response := requestPreflight(ctx, router, tc.provider, "?node=localai")
			require.Equal(t, tc.status, response.Code, response.Body.String())
			assert.Contains(t, response.Header().Get("Content-Type"), "application/problem+json")
			var problem struct{ Code string }
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			assert.Equal(t, tc.code, problem.Code)
			assertPreflightRoute(t, router, tc.provider, "localai")
			assert.Zero(t, inst.calls)
		})
	}
}
