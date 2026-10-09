package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareLocalLoadRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		enabled  bool
		cause    string
		status   int
	}{
		{"unknown provider", "missing", true, "unknown provider: missing", http.StatusBadRequest},
		{"disabled provider", "fake", false, "provider not enabled: fake", http.StatusBadRequest},
		{"missing weights", "fake", true, "Model file not found", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, _, launches := newAliasLoadExecutor(t)
			executor.isProviderEnabled = func(string) bool { return tc.enabled }
			req := &LoadModelRequest{ModelName: aliasLoadRepoID, Provider: tc.provider}
			prepared, refusal, err := executor.prepareLocalLoad(t.Context(), req)
			require.NoError(t, err)
			assert.Nil(t, prepared)
			require.NotNil(t, refusal)
			assert.Equal(t, tc.status, refusal.StatusCode)
			assert.Equal(t, tc.cause, refusal.Error)
			response, err := executor.LoadLocalModel(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, refusal, response)
			assert.Zero(t, *launches)
			assert.Equal(t, aliasLoadRepoID, req.ModelName)
		})
	}
}

func TestLoadLocalModelValidatesEachNameBeforeDedupe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, err := prov_apps.NewProviderAppManager(aliasLoadConfig(t),
			prov_apps.WithModelCanonicalizer(func(string) string { return "canonical" }),
			prov_apps.WithModelValidator(func(_ context.Context, model string) error {
				if model == "conflicting" {
					return config.ErrModelNameConflict
				}
				return nil
			}),
		)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, manager.Stop(context.Background())) })
		release := make(chan struct{})
		executor := NewLoadExecutor(manager, func(string) bool { return true }, emptyModelStore{},
			func(string) (*backend.Resolved, bool) { return nil, false },
			func(_ context.Context, model, provider, _ string, _, _ map[string]string) (*instance.Instance, error) {
				<-release
				return instance.NewInstance("fake", provider, model, 0, 0, 0), nil
			}, func() string { return "node" }, nil)
		valid := make(chan error, 1)
		go func() {
			_, err := executor.LoadLocalModel(t.Context(), &LoadModelRequest{ModelName: "valid", Provider: "fake", Force: true})
			valid <- err
		}()
		synctest.Wait()
		refused := make(chan error, 1)
		go func() {
			_, err := executor.LoadLocalModel(t.Context(), &LoadModelRequest{ModelName: "conflicting", Provider: "fake", Force: true})
			refused <- err
		}()
		synctest.Wait()
		select {
		case err := <-refused:
			assert.ErrorIs(t, err, config.ErrModelNameConflict)
		default:
			t.Error("each original name must be admitted before joining a launch")
		}
		close(release)
		synctest.Wait()
		require.NoError(t, <-valid)
		select {
		case err := <-refused:
			assert.True(t, errors.Is(err, config.ErrModelNameConflict))
		default:
		}
	})
}
