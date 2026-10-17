package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
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

func runParameterProblem(t *testing.T, body []byte) utils.ProblemDetails {
	t.Helper()
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(body, &problem), string(body))
	return problem
}

// An agent spelled llama.cpp's context parameter the way another engine
// does. Every launch surface refuses it before a process exists, and the
// refusal names the key the provider takes instead.
func TestRunParametersRefuseUndeclaredContextLength(t *testing.T) {
	s := variantNode(t)
	for _, path := range []string{"/zzrouter/v1/runs/preview", "/zzrouter/v1/runs", "/zzrouter/v1/runs/load"} {
		t.Run(path, func(t *testing.T) {
			body := `{"model":"` + qwenModel + `","provider":"llamacpp","parameters":{"context_length":32768}}`
			if path == "/zzrouter/v1/runs" {
				body = `{"model":"` + qwenModel + `","provider":"llamacpp","launch_mode":"native","parameters":{"context_length":32768}}`
			}
			w := assetRequest(t, s, http.MethodPost, path, []byte(body))
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, w.Header().Get("Content-Type"), "application/problem+json")
			problem := runParameterProblem(t, w.Body.Bytes())
			assert.Equal(t, string(httperr.CodeUnknownFlag), problem.Code)
			require.Len(t, problem.Errors, 1, w.Body.String())
			issue := problem.Errors[0]
			assert.Equal(t, "context_length", issue.Key)
			assert.Equal(t, string(httperr.CodeUnknownFlag), issue.Code)
			assert.Contains(t, issue.Message, "request")
			assert.Contains(t, issue.Hint, "ctx-size")
			assert.Contains(t, issue.Hint, "zz_params_get")
			assert.NotContains(t, w.Body.String(), "--context-length")
		})
	}
	assert.Empty(t, s.providers.appMgr.ListInstances(""))
	assert.Empty(t, s.providers.appMgr.Ports().ListAllAllocated())

	result := callMCPWireTool(t, s, "zz_run_preview", map[string]any{"model": qwenModel, "provider": "llamacpp", "parameters": map[string]any{"context_length": 32768}})
	require.Equal(t, true, result["isError"])
	structured := result["structuredContent"].(map[string]any)
	assert.Equal(t, string(httperr.CodeUnknownFlag), structured["code"])
	issue := structured["errors"].([]any)[0].(map[string]any)
	assert.Equal(t, "context_length", issue["key"])
	assert.Contains(t, issue["hint"], "ctx-size")
}

// Values meet the same rules PATCH holds a persisted value to, and a key
// a configured tier set is refused with that tier named, not passed on.
func TestRunParametersCheckEveryTier(t *testing.T) {
	s := variantNode(t)
	preview := func(parameters string) utils.ProblemDetails {
		t.Helper()
		w := assetRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", []byte(`{"model":"`+qwenModel+`","provider":"llamacpp","parameters":`+parameters+`}`))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		return runParameterProblem(t, w.Body.Bytes())
	}
	for parameters, code := range map[string]httperr.ParamErrorCode{
		`{"ctx-size":"many"}`:       httperr.CodeWrongType,
		`{"ctx-size":-5}`:           httperr.CodeOutOfRange,
		`{"keep-alive":"soon"}`:     httperr.CodeCoercionFailed,
		`{"embeddings":true}`:       httperr.CodeUnknownFlag,
		`{"chat-template-file":""}`: httperr.CodeInvalidValue,
	} {
		problem := preview(parameters)
		require.Len(t, problem.Errors, 1, parameters)
		assert.Equal(t, string(code), problem.Errors[0].Code, parameters)
		assert.Equal(t, string(code), problem.Code, parameters)
	}
	mixed := preview(`{"context_length":1,"ctx-size":0}`)
	require.Len(t, mixed.Errors, 2)
	assert.Equal(t, "bad_request", mixed.Code, "entries that disagree leave the status code")

	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(svc *config.ServiceConfig) error {
		spec := svc.Models[qwenModel]
		spec.Parameters = map[string]string{"context-length": "8192"}
		if svc.Models == nil {
			svc.Models = map[string]config.ModelSpec{}
		}
		svc.Models[qwenModel] = spec
		return nil
	}))
	configured := preview(`{}`)
	require.Len(t, configured.Errors, 1)
	assert.Equal(t, "context-length", configured.Errors[0].Key)
	assert.Contains(t, configured.Errors[0].Message, "model tier")
}

// A file feature's flag is declared by the schema builder both PATCH and
// launch read: it takes the opt-out the feature docs promise, never a path.
func TestFeatureFlagTakesOnlyAnOptOut(t *testing.T) {
	s := variantNode(t)
	shape := loadProviderSchema(s.configStore, "llamacpp").Parameters["mmproj"]
	assert.Equal(t, []string{"auto", "false"}, shape.Enum)
	for _, value := range []string{"auto", "false"} {
		code, body := patchParams(t, s, map[string]any{"defaults": map[string]any{"parameters": map[string]any{"mmproj": value}}})
		require.Equal(t, http.StatusOK, code, body)
		w := assetRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", []byte(`{"model":"`+qwenModel+`","provider":"llamacpp","parameters":{"mmproj":"`+value+`"}}`))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	code, body := patchParams(t, s, map[string]any{"defaults": map[string]any{"parameters": map[string]any{"mmproj": "/models/projector.gguf"}}})
	require.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, string(httperr.CodeCoercionFailed))
	w := assetRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", []byte(`{"model":"`+qwenModel+`","provider":"llamacpp","parameters":{"mmproj":"/models/projector.gguf"}}`))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, string(httperr.CodeCoercionFailed), runParameterProblem(t, w.Body.Bytes()).Code)

	declared := filepath.Join(s.configStore.DirPath(), "on-demand", "llamacpp", "schema.yaml")
	require.NoError(t, os.WriteFile(declared, []byte("provider: llamacpp\nparameters:\n  mmproj: {type: string, description: declared}\n"), 0o600))
	assert.Equal(t, "declared", loadProviderSchema(s.configStore, "llamacpp").Parameters["mmproj"].Description, "a schema that declares the flag keeps its declaration")
}
