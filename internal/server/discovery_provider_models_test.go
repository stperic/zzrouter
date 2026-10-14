package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIDiscoveryProviderModels(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	resp := makeAuthRequest(t, s, http.MethodGet, apiDiscoveryPrefix, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code)
	var out struct {
		Data apiDiscoveryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &out))
	require.Contains(t, out.Data.ProviderModels, "Coordinator admin")
	require.Contains(t, out.Data.ProviderModels, "docs/comfyui.md")
	require.Contains(t, out.Data.ProviderModels, "job_id")
	var advertised []string
	for _, group := range out.Data.EndpointGroups {
		advertised = append(advertised, group...)
	}
	for _, route := range []string{"GET /zzrouter/v1/providers/{name}/models", "POST /zzrouter/v1/providers/{name}/models", "DELETE /zzrouter/v1/providers/{name}/models", "POST /zzrouter/v1/providers/{name}/models/resolve"} {
		require.Contains(t, advertised, route)
	}
}
