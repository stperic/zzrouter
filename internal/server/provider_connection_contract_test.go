package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnectionProblemsHaveActionableCodes(t *testing.T) {
	for _, tc := range []struct {
		name, method, provider, code string
		status                       int
		prepare                      func(*testing.T, *connectionHarness)
	}{
		{"unknown provider", "GET", "absent", "provider_connection_unsupported", 404, nil},
		{"undeclared connection", "GET", "fixture", "provider_connection_unsupported", 404, func(t *testing.T, h *connectionHarness) {
			require.NoError(t, h.config.ApplyParameterPatch("fixture", func(s *config.ServiceConfig) error { s.Connection = nil; return nil }))
		}},
		{"unavailable config", "GET", "fixture", "provider_connection_unavailable", 503, func(_ *testing.T, h *connectionHarness) {
			h.service.resolver = backend.NewResolver(func() *config.AppsConfig { return nil }, h.vault)
		}},
		{"unavailable resolver", "GET", "fixture", "provider_connection_unavailable", 503, func(_ *testing.T, h *connectionHarness) { h.service.resolver = nil }},
		{"closed protected store", "GET", "fixture", "provider_connection_unavailable", 503, func(t *testing.T, h *connectionHarness) {
			require.NoError(t, h.vault.Close())
		}},
		{"missing protection", "PUT", "fixture", "managed_credentials_unavailable", 503, func(_ *testing.T, h *connectionHarness) { h.service.store = nil }},
		{"stale revision", "PUT", "fixture", "provider_connection_conflict", 409, func(t *testing.T, h *connectionHarness) {
			_, err := h.vault.Remove(t.Context(), "fixture", 0)
			require.NoError(t, err)
		}},
		{"persistence failure", "PUT", "fixture", "provider_connection_persistence_failed", 503, func(_ *testing.T, h *connectionHarness) {
			h.service.store = failedConnectionStore{connectionStore: h.vault, err: errors.New("private persistence details")}
		}},
		{"wrong coordinator", "GET", "fixture", "provider_connection_scope", 409, func(_ *testing.T, h *connectionHarness) { h.service.coordinator = func() bool { return false } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConnectionHarness(t, "https://fixture.example")
			if tc.prepare != nil {
				tc.prepare(t, h)
			}
			body := []byte(`{"expected_revision":0,"values":{"endpoint":"https://fixture.example","token":"candidate-console-fixture"}}`)
			rec := httptest.NewRecorder()
			h.engine.ServeHTTP(rec, httptest.NewRequest(tc.method, "/zzrouter/v1/providers/"+tc.provider+"/connection", bytes.NewReader(body)))
			assert.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
			var problem utils.ProblemDetails
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
			assert.Equal(t, tc.code, problem.Code)
			assert.NotContains(t, rec.Body.String(), "candidate-console-fixture")
			assert.NotContains(t, rec.Body.String(), "private persistence details")
		})
	}
}

func TestConnectionInvalidCandidateDoesNotEchoValues(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			h := newConnectionHarness(t, "https://fixture.example")
			path := "/zzrouter/v1/providers/fixture/connection"
			body := `{"expected_revision":0,"values":{"endpoint":"https://fixture.example","token":"candidate-console-fixture","unknown":"private-extra-value"}}`
			if method == http.MethodPost {
				path += "/test"
				body = `{"values":{"endpoint":"https://fixture.example","token":"candidate-console-fixture","unknown":"private-extra-value"}}`
			}
			rec := httptest.NewRecorder()
			h.engine.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
			var problem utils.ProblemDetails
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
			assert.Equal(t, 400, rec.Code)
			assert.Equal(t, "invalid_provider_connection", problem.Code)
			assert.NotContains(t, rec.Body.String(), "candidate-console-fixture")
			assert.NotContains(t, rec.Body.String(), "private-extra-value")
			state, err := h.vault.Snapshot("fixture")
			require.NoError(t, err)
			assert.Zero(t, state.Revision)
		})
	}
}

func TestConnectionCandidateRejectsChangedAvailability(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			h := newConnectionHarness(t, "https://fixture.example")
			reads := 0
			h.service.resolver = backend.NewResolver(func() *config.AppsConfig {
				reads++
				if reads > 1 {
					return nil
				}
				return h.config.Config()
			}, h.vault)
			path := "/zzrouter/v1/providers/fixture/connection"
			body := `{"expected_revision":0,"values":{"endpoint":"https://fixture.example","token":"candidate-console-fixture"}}`
			if method == http.MethodPost {
				path += "/test"
				body = `{"values":{"endpoint":"https://fixture.example","token":"candidate-console-fixture"}}`
			}
			rec := httptest.NewRecorder()
			h.engine.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
			var problem utils.ProblemDetails
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
			assert.Equal(t, 503, rec.Code)
			assert.Equal(t, "provider_connection_unavailable", problem.Code)
			state, err := h.vault.Snapshot("fixture")
			require.NoError(t, err)
			assert.Zero(t, state.Revision)
		})
	}
}

func TestProviderCatalogConnectionContract(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	// Catalog capability comes from declarations; an empty resolver avoids
	// looking up the developer's external credentials while testing templates.
	ctrl := &ProvidersController{appsConfig: func() *config.AppsConfig { return cfg }, backend: backend.NewResolver(func() *config.AppsConfig { return &config.AppsConfig{} })}
	engine := gin.New()
	engine.GET("/zzrouter/v1/providers/catalog", ctrl.ListProviderCatalog)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest("GET", "/zzrouter/v1/providers/catalog?kind=all", nil))
	require.Equal(t, 200, rec.Code)
	var listing struct {
		Data []struct {
			Key                 string `json:"key"`
			ConnectionSupported *bool  `json:"connection_supported"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listing))
	want := map[string]bool{"openai": true, "anthropic": true, "gemini": true, "groq": true, "openrouter": true, "azure": true, "bedrock": true, "cloudflare": true, "ollama-cloud": true, "huggingface": true}
	for _, entry := range listing.Data {
		require.NotNil(t, entry.ConnectionSupported, entry.Key)
		assert.Equal(t, want[entry.Key], *entry.ConnectionSupported, entry.Key)
		delete(want, entry.Key)
	}
	assert.Empty(t, want, "every supported cloud and Hugging Face is listed")
	server := httptest.NewServer(engine)
	defer server.Close()
	api := client.NewClient(config.ClientNodeConfig{Address: server.URL})
	entries, err := api.GetProviderCatalog("all")
	require.NoError(t, err)
	require.Len(t, entries, len(listing.Data))
	for i, entry := range entries {
		assert.Equal(t, listing.Data[i].Key, entry.Key)
		assert.Equal(t, *listing.Data[i].ConnectionSupported, entry.ConnectionSupported, entry.Key)
	}

	cfg = nil
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest("GET", "/zzrouter/v1/providers/catalog?kind=all", nil))
	assert.Equal(t, 503, rec.Code)
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, "provider_configuration_unavailable", problem.Code)

	cfg = &config.AppsConfig{}
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest("GET", "/zzrouter/v1/providers/catalog?kind=all", nil))
	assert.Equal(t, 200, rec.Code)
	var empty ListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &empty))
	assert.Equal(t, []any{}, empty.Data, "an empty catalog is an array, never null")
	assert.Zero(t, empty.Total)
	assert.False(t, empty.HasMore)
}
