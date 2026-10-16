package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

func TestListModelsPreservesRemoteIdentityAndCredential(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		assert.Equal(t, "Bearer provider-token", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"org/model:tag@part#file","status":"unloaded"},{"id":"alias","loaded":true}]}`))
	}))
	t.Cleanup(daemon.Close)
	api := &config.APIConfig{AuthType: config.AuthTypeBearer, AuthHeader: "Authorization", AuthPrefix: "Bearer ", Token: "provider-token"}
	models, err := ListModels(t.Context(), &backend.Resolved{Endpoint: daemon.URL, Upstream: backend.ForProvider(api)}, "strata")
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "org/model:tag@part#file", models[0].Name)
	assert.Equal(t, models[0].Name, models[0].SourceID)
	assert.Equal(t, metadata.SourceExternalOpenAI, models[0].SourceRepo)
	assert.Equal(t, "strata", models[0].AssignedApp)
	assert.Equal(t, daemon.URL, models[0].Backend)
	assert.Empty(t, models[0].FullPath)
	assert.Equal(t, "unloaded", models[0].Extra["details"].(map[string]any)["upstream_status"])
	assert.Equal(t, true, models[1].Extra["details"].(map[string]any)["upstream_loaded"])
}

func TestListModelsRejectsInvalidOrOversizedInventory(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{name: "missing data", body: `{}`},
		{name: "null data", body: `{"data":null}`},
		{name: "object data", body: `{"data":{}}`},
		{name: "blank id", body: `{"data":[{"id":" "}]}`},
		{name: "numeric id", body: `{"data":[{"id":123}]}`},
		{name: "trailing document", body: `{"data":[]} {}`},
		{name: "too large", body: `{"data":[]}` + strings.Repeat(" ", maxListBody)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(daemon.Close)
			_, err := ListModels(t.Context(), &backend.Resolved{Endpoint: daemon.URL}, "strata")
			require.Error(t, err)
		})
	}
}

func TestListModelsCancellation(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(daemon.Close)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ListModels(ctx, &backend.Resolved{Endpoint: daemon.URL}, "strata")
	require.ErrorIs(t, err, context.Canceled)
}

func TestListModelsIgnoresUnknownOptionalFields(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m","status":{"state":"unloaded"},"loaded":"later","aliases":["alias"]}]}`))
	}))
	t.Cleanup(daemon.Close)
	models, err := ListModels(t.Context(), &backend.Resolved{Endpoint: daemon.URL}, "strata")
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "m", models[0].Name)
	require.Empty(t, models[0].Extra)
}
