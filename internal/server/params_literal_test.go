package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stretchr/testify/require"
)

func TestPatchLiteralFlagSpelling(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: DefaultTestNodeConfig(), providers: []string{"comfyui"}, enabled: []string{"comfyui"}})
	patch := func(body string) *TestResponse {
		return makeAuthRequest(t, s, http.MethodPatch, apipath.ProviderParameters("comfyui"), TestAdminKey, json.RawMessage(body))
	}
	for _, key := range []string{"fp8-e4m3fn-unet", "fp8_e4m3fn_unet"} {
		response := patch(`{"defaults":{"parameters":{"` + key + `":true}}}`)
		require.Equal(t, http.StatusOK, response.Code, string(response.Body))
	}
	sc, ok := s.appsConfig().LookupApp("comfyui")
	require.True(t, ok)
	count := 0
	for key := range sc.Defaults.Parameters {
		if key == "fp8-e4m3fn-unet" || key == "fp8_e4m3fn_unet" {
			count++
		}
	}
	require.Equal(t, 1, count)
	require.Contains(t, sc.Defaults.Parameters, "fp8-e4m3fn-unet")
	response := patch(`{"defaults":{"parameters":{"fp8-e4m3fn-unet":true,"fp8_e4m3fn-unet":false}}}`)
	require.Equal(t, http.StatusBadRequest, response.Code, string(response.Body))
	problem := runParameterProblem(t, response.Body)
	require.Len(t, problem.Errors, 2)
	for _, issue := range problem.Errors {
		require.Equal(t, string(httperr.CodeDuplicateFlag), issue.Code)
	}
}
