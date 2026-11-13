package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryConnectionRefusalWire(t *testing.T) {
	for _, state := range []string{"unconfigured", "destination-mismatch"} {
		t.Run(state, func(t *testing.T) {
			h := newConnectionHarness(t, "https://original.example.test")
			if state == "unconfigured" {
				t.Setenv("CONNECTION_TEST_TOKEN", "")
			} else {
				_, err := h.service.Save(t.Context(), "fixture", 0, map[string]string{"endpoint": "https://original.example.test", "token": "synthetic"}, false)
				require.NoError(t, err)
				require.NoError(t, h.config.ApplyParameterPatch("fixture", func(s *config.ServiceConfig) error { s.Runtime.Endpoint = "https://moved.example.test"; return nil }))
			}
			search.RegisterCloudProvidersFromConfig(h.config.Config, h.service.resolver)
			svc := NewModelSearchService(h.config.Config, nil, h.service.resolver)
			h.engine.GET(apipath.Search, svc.HandleSearchPublic)
			h.engine.GET(apipath.ModelCard(":provider", "*id"), svc.handleGetModelCard)
			for _, endpoint := range []struct{ name, path string }{
				{"search", apipath.Search + "?type=models&provider=fixture"},
				{"popular", apipath.Search + "?type=popular&provider=fixture"},
				{"card", apipath.ModelCard("fixture", "model")},
			} {
				t.Run(endpoint.name, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodGet, endpoint.path, nil)
					rec := httptest.NewRecorder()
					h.engine.ServeHTTP(rec, req)
					require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
					var problem utils.ProblemDetails
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
					assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
					assert.Equal(t, string(httperr.CodeProviderConnectionUnavailable), problem.Code)
					require.Len(t, problem.Errors, 1)
					assert.Equal(t, "provider", problem.Errors[0].Key)
					assert.Equal(t, "fixture", problem.Errors[0].Got)
					assert.Contains(t, problem.Errors[0].Hint, `zz_api_search(query="ProviderConnection")`)
					assert.NotContains(t, rec.Body.String(), "synthetic")
					assert.NotContains(t, rec.Body.String(), "original.example.test")
				})
			}
		})
	}
}
