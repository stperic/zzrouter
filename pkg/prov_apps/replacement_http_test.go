package prov_apps_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/server"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestRunStartReplaceMemoryRefusalKeepsOldRunServing(t *testing.T) {
	mgr, old := prov_apps.ReplacementRefusalFixture(t)
	executor := server.NewRunsExecutor(mgr, func() string { return "worker" }, mgr.AppsConfig, nil, nil, nil)
	internal := gin.New()
	internal.POST("/zzrouter/v1/internal/runs", executor.HandleInternalLaunchRun)
	router := routing.NewLocalOnlyRouterWithClient("worker", "", func(ctx context.Context, req *routing.Request) (*routing.Response, error) {
		w := httptest.NewRecorder()
		request := httptest.NewRequestWithContext(ctx, req.Method, req.Path, bytes.NewReader(req.Body))
		request.Header.Set("Content-Type", "application/json")
		internal.ServeHTTP(w, request)
		return &routing.Response{StatusCode: w.Code, Headers: w.Header(), Body: w.Body.Bytes()}, nil
	}, nil)
	controller := server.NewRunsController(server.NewRunsService(router), nil, nil, nil, nil)
	public := gin.New()
	public.POST("/zzrouter/v1/runs", controller.LaunchRun)
	w := httptest.NewRecorder()
	public.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/zzrouter/v1/runs",
		strings.NewReader(`{"model":"shared","provider":"generic","node":"worker","replace":true}`)))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))
	require.Equal(t, "out_of_range", problem.Code)
	require.Len(t, problem.Errors, 1)
	got, ok := problem.Errors[0].Got.(map[string]any)
	require.True(t, ok)
	require.Equal(t, old.ID, got["kept_run"])
	require.Equal(t, float64(2000), got["credited_mib"])
	require.Equal(t, instance.StatusRunning, old.GetStatus())
	lease, err := old.Acquire(t.Context(), nil)
	require.NoError(t, err)
	lease.Release()
}
