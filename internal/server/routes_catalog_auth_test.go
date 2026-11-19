package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stretchr/testify/require"
)

func TestRouteCatalog_OllamaMutationAuthentication(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	response := makeAuthRequest(t, s, http.MethodGet, apipath.ServerRoutes, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, response.Code, string(response.Body))
	var catalog struct {
		Routes []RouteSpec `json:"routes"`
	}
	require.NoError(t, json.Unmarshal(response.Body, &catalog))
	wantAdmin := map[string]bool{
		"POST /api/pull": true, "POST /api/create": true, "POST /api/copy": true,
		"POST /api/push": true, "DELETE /api/delete": true, "POST /api/blobs/:digest": true,
	}
	checked, mutations := 0, 0
	for _, route := range catalog.Routes {
		if route.Surface != "ollama" {
			continue
		}
		checked++
		want := "optional"
		if wantAdmin[route.Method+" "+route.Path] {
			want = "admin"
			mutations++
		}
		require.Equal(t, want, route.Auth, "%s %s", route.Method, route.Path)
	}
	require.Equal(t, len(wantAdmin), mutations)
	require.Greater(t, checked, mutations, "read and inference siblings keep their existing audience")
}
