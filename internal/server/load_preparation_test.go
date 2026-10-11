package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadLocalModelRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		local          bool
		cause          error
		status         int
	}{
		{"unknown provider", "missing", true, prov_apps.ErrProviderNotFound, http.StatusNotFound},
		{"missing weights", "fake", false, prov_apps.ErrModelNotLocal, http.StatusNotFound},
		{"disabled provider", "fake", true, prov_apps.ErrServiceDisabled, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, manager, launches := newAliasLoadExecutor(t)
			prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) { return tc.local, nil })(manager)
			if tc.cause == prov_apps.ErrServiceDisabled {
				cfg := aliasLoadConfig(t)
				svc, _ := cfg.LookupApp("fake")
				svc.Enabled = new(false)
				require.NoError(t, cfg.UpdateApp("fake", func(dst *config.ServiceConfig) error { *dst = svc; return nil }))
				manager.ReloadConfig(cfg)
			}
			req := &LoadModelRequest{ModelName: aliasLoadRepoID, Provider: tc.provider}
			response, err := executor.LoadLocalModel(t.Context(), req)
			require.ErrorIs(t, err, tc.cause)
			assert.Nil(t, response)
			assert.Equal(t, tc.status, answeredStatus(err))
			assert.Zero(t, *launches)
			assert.Equal(t, aliasLoadRepoID, req.ModelName)
		})
	}
}

func TestLoadLocalModelValidatesEachNameBeforeDedupe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		stopped := errors.New("test stops before process start")
		manager, err := prov_apps.NewProviderAppManager(aliasLoadConfig(t),
			prov_apps.WithModelCanonicalizer(func(string) string { return "canonical" }),
			prov_apps.WithModelValidator(func(_ context.Context, model string) error {
				if model == "conflicting" {
					return config.ErrModelNameConflict
				}
				return nil
			}),
			prov_apps.WithModelSource(func(context.Context, string) string { <-release; return "" }),
			prov_apps.WithParamLocalizer(func(_, _, _ string, _ map[string]string) (prov_apps.Localized, error) {
				return prov_apps.Localized{}, stopped
			}),
		)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, manager.Stop(context.Background())) })
		executor := NewLoadExecutor(manager, emptyModelStore{}, func() string { return "node" })
		valid := make(chan error, 1)
		go func() {
			_, err := executor.LoadLocalModel(t.Context(), &LoadModelRequest{ModelName: "valid", Provider: "fake"})
			valid <- err
		}()
		synctest.Wait()
		_, err = executor.LoadLocalModel(t.Context(), &LoadModelRequest{ModelName: "conflicting", Provider: "fake"})
		require.ErrorIs(t, err, config.ErrModelNameConflict)
		close(release)
		require.ErrorIs(t, <-valid, stopped)
	})
}
