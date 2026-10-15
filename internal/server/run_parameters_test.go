package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestRunParameterBoundaries(t *testing.T) {
	RegisterValidatorTranslator()
	for _, surface := range []struct {
		name       string
		request    func() any
		parameters func(any) schema.ParameterValues
	}{
		{"launch", func() any { return &LaunchRunRequest{} }, func(r any) schema.ParameterValues { return r.(*LaunchRunRequest).Parameters }},
		{"preview and load", func() any { return &LoadModelRequest{} }, func(r any) schema.ParameterValues { return r.(*LoadModelRequest).Parameters }},
		{"batch member", func() any { return &LaunchRequest{} }, func(r any) schema.ParameterValues { return r.(*LaunchRequest).Parameters }},
	} {
		t.Run(surface.name, func(t *testing.T) {
			req := surface.request()
			body := `{"provider":"studio","launch_mode":"native","parameters":{"gpu-reserve-mib":16384,"bool":false,"large":9007199254740993,"decimal":0.1234567890123456789,"str":"auto"}}`
			if surface.name == "preview and load" {
				body = `{"provider":"studio","parameters":{"gpu-reserve-mib":16384,"bool":false,"large":9007199254740993,"decimal":0.1234567890123456789,"str":"auto"}}`
			}
			c, rec := postJSON(t, body)
			require.True(t, BindJSONStrict(c, req), rec.Body.String())
			require.Equal(t, schema.ParameterValues{"gpu-reserve-mib": "16384", "bool": "false", "large": "9007199254740993", "decimal": "0.1234567890123456789", "str": "auto"}, surface.parameters(req))
			for _, value := range []string{"null", "{}", "[]"} {
				c, rec := postJSON(t, `{"provider":"studio","launch_mode":"native","parameters":{"bad":`+value+`}}`)
				if surface.name == "preview and load" {
					c, rec = postJSON(t, `{"parameters":{"bad":`+value+`}}`)
				}
				require.False(t, BindJSONStrict(c, surface.request()))
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
				var problem utils.ProblemDetails
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
				require.Equal(t, "bad_request", problem.Code)
				require.Contains(t, problem.Detail, "parameters.bad")
				require.Contains(t, problem.Detail, "string, number or boolean")
			}
		})
	}
}

func TestBatchRunParametersAcceptScalars(t *testing.T) {
	var batch BatchLaunchRequest
	require.NoError(t, json.Unmarshal([]byte(`{"instances":[{"provider":"studio","parameters":{"gpu-reserve-mib":16384,"flag":true}}]}`), &batch))
	require.Equal(t, schema.ParameterValues{"gpu-reserve-mib": "16384", "flag": "true"}, batch.Instances[0].Parameters)
}

func TestRunParametersDoNotWidenEnvironment(t *testing.T) {
	for _, req := range []any{&LaunchRunRequest{}, &LoadModelRequest{}} {
		body := `{"provider":"studio","env_vars":{"BAD":true}}`
		if _, load := req.(*LoadModelRequest); load {
			body = `{"provider":"studio","environment":{"BAD":true}}`
		}
		c, rec := postJSON(t, body)
		require.False(t, BindJSONStrict(c, req))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "expected string")
	}
}

func TestRunParameterOpenAPISchemas(t *testing.T) {
	doc := loadSpecTree(t)
	components := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, name := range []string{"LaunchRunRequest", "PreviewRunRequest", "LoadModelRequest"} {
		properties := components[name].(map[string]any)["properties"].(map[string]any)
		parameters := properties["parameters"].(map[string]any)
		require.Equal(t, "object", parameters["type"])
		value := parameters["additionalProperties"].(map[string]any)
		require.Equal(t, []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}, map[string]any{"type": "boolean"}}, value["oneOf"], name)
	}
}
