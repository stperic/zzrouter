package server

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireStartProblem(t *testing.T, err error, status int, code string) *utils.ProblemDetails {
	t.Helper()
	var routed *RoutedError
	require.ErrorAs(t, err, &routed)
	require.NotNil(t, routed.ProblemDetails)
	require.Equal(t, status, routed.StatusCode)
	require.Equal(t, status, routed.ProblemDetails.Status)
	require.Equal(t, code, routed.ProblemDetails.Code)
	return routed.ProblemDetails
}

func TestStartRunRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		local          bool
		cause          error
		status         int
		code           string
	}{
		{"unknown provider", "missing", true, prov_apps.ErrProviderNotFound, http.StatusNotFound, "not_found"},
		{"missing weights", "fake", false, prov_apps.ErrModelNotLocal, http.StatusNotFound, "not_found"},
		{"disabled provider", "fake", true, prov_apps.ErrServiceDisabled, http.StatusConflict, "conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, manager, launches := newAliasRunsExecutor(t)
			prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) { return tc.local, nil })(manager)
			if tc.cause == prov_apps.ErrServiceDisabled {
				cfg := aliasLoadConfig(t)
				svc, _ := cfg.LookupApp("fake")
				svc.Enabled = new(false)
				require.NoError(t, cfg.UpdateApp("fake", func(dst *config.ServiceConfig) error { *dst = svc; return nil }))
				manager.ReloadConfig(cfg)
			}
			req := LaunchRunRequest{Model: aliasLoadRepoID, Provider: tc.provider}
			response, err := startForTest(t, t.Context(), executor, req)
			problem := requireStartProblem(t, err, tc.status, tc.code)
			assert.Contains(t, problem.Detail, tc.cause.Error())
			assert.Nil(t, response)
			assert.Zero(t, *launches)
			assert.Equal(t, aliasLoadRepoID, req.Model)
		})
	}
}

func TestStartRunValidatesEachNameBeforeDedupe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		manager, err := prov_apps.NewProviderAppManager(aliasLoadConfig(t),
			prov_apps.WithModelCanonicalizer(func(string) string { return "canonical" }),
			prov_apps.WithModelValidator(func(_ context.Context, model string) error {
				if model == "conflicting" {
					return config.ErrModelNameConflict
				}
				return nil
			}),
			prov_apps.WithModelSource(func(context.Context, string) string { <-release; return "" }),
			prov_apps.WithParamLocalizer(func(_, _, _ string, _ map[string]string, _ map[string]schema.ParamShape) (prov_apps.Localized, error) {
				return prov_apps.Localized{}, prov_apps.ErrModelIncomplete
			}),
		)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, manager.Stop(context.Background())) })
		executor := NewRunsExecutor(manager, func() string { return "node" }, manager.AppsConfig, nil, nil, nil)
		valid := make(chan error, 1)
		go func() {
			_, err := startForTest(t, t.Context(), executor, LaunchRunRequest{Model: "valid", Provider: "fake"})
			valid <- err
		}()
		synctest.Wait()
		_, err = startForTest(t, t.Context(), executor, LaunchRunRequest{Model: "conflicting", Provider: "fake"})
		close(release)
		requireStartProblem(t, err, http.StatusConflict, "model_name_conflict")
		requireStartProblem(t, <-valid, http.StatusConflict, "model_incomplete")
	})
}
