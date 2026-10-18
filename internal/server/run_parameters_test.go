package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps"
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
			assert.Contains(t, issue.Message, `"context_length"`, "the message names the key as sent")
			assert.NotContains(t, problem.Detail, "context-length")
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
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Equal(t, 1, strings.Count(text, "Its context length parameter is ctx-size."), "the text carries the hint once: %s", text)
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

	// The refusal names the key as stored, the one the hint's null removes.
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(svc *config.ServiceConfig) error {
		svc.Models[qwenModel] = config.ModelSpec{Parameters: map[string]string{"context_length": "8192"}}
		return nil
	}))
	stored := preview(`{}`)
	require.Len(t, stored.Errors, 1)
	assert.Equal(t, "context_length", stored.Errors[0].Key)
	assert.Contains(t, stored.Errors[0].Hint, "null removes it")
	assert.Contains(t, resolvedFor(t, s, "llamacpp", qwenModel).Parameters, "context_length", "the resolved view lists it as stored too")
	code, body := patchParams(t, s, map[string]any{"models": map[string]any{qwenModel: map[string]any{"parameters": map[string]any{"context_length": nil}}}})
	require.Equal(t, http.StatusOK, code, body)
	w := assetRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", []byte(`{"model":"`+qwenModel+`","provider":"llamacpp"}`))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// An integral JSON number is an integer wherever JSON becomes a parameter's
// string: a PATCH stores 32768.0 as 32768 and a request's 1e6 reaches argv
// as 1000000, while a numeric string an int parser rejects is wrong_type.
func TestIntegralNumbersBecomeIntegerLiterals(t *testing.T) {
	s := variantNode(t)
	resp := makeAuthRequest(t, s, http.MethodPatch, apipath.ProviderParameters("llamacpp"), TestAdminKey, json.RawMessage(`{"defaults":{"parameters":{"ctx-size":32768.0}}}`))
	require.Equal(t, http.StatusOK, resp.Code, string(resp.Body))
	svc, _ := s.appsConfig().LookupApp("llamacpp")
	assert.Equal(t, "32768", svc.Defaults.Parameters["ctx-size"])
	for value, code := range map[string]httperr.ParamErrorCode{"3.5": httperr.CodeWrongType, "1e19": httperr.CodeOutOfRange} {
		resp := makeAuthRequest(t, s, http.MethodPatch, apipath.ProviderParameters("llamacpp"), TestAdminKey, json.RawMessage(`{"defaults":{"parameters":{"ctx-size":`+value+`}}}`))
		require.Equal(t, http.StatusBadRequest, resp.Code, string(resp.Body))
		problem := runParameterProblem(t, resp.Body)
		require.Len(t, problem.Errors, 1, value)
		assert.Equal(t, string(code), problem.Errors[0].Code, "a JSON number the int parser refuses: %s", value)
	}

	w := assetRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", []byte(`{"model":"`+qwenModel+`","provider":"llamacpp","parameters":{"ctx-size":1e6}}`))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var preview struct {
		Data PreviewRunResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &preview))
	assert.Contains(t, preview.Data.FullCommand, "--ctx-size 1000000")
	w = assetRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", []byte(`{"model":"`+qwenModel+`","provider":"llamacpp","parameters":{"ctx-size":"3.2768e+04"}}`))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, string(httperr.CodeWrongType), runParameterProblem(t, w.Body.Bytes()).Code)
}

// A PATCH addresses a flag, whatever spelling stored it: it validates
// ctx_size as ctx-size, stores the flag name in place of every spelling, and
// a null removes them all, so no write leaves a launch refusing
// duplicate_flag. Two spellings in one patched map are refused, each by name.
func TestPatchAddressesTheFlag(t *testing.T) {
	s := variantNode(t)
	store := func(params map[string]string) {
		require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(svc *config.ServiceConfig) error {
			defaults := *svc.Defaults
			defaults.Parameters = params
			svc.Defaults = &defaults
			return nil
		}))
	}
	stored := func() map[string]string {
		svc, _ := s.appsConfig().LookupApp("llamacpp")
		return svc.Defaults.Parameters
	}
	preview := func() int {
		return assetRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", []byte(`{"model":"`+qwenModel+`","provider":"llamacpp"}`)).Code
	}

	store(map[string]string{"ctx_size": "8192"})
	code, body := patchParams(t, s, map[string]any{"defaults": map[string]any{"parameters": map[string]any{"ctx_size": 8192}}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, map[string]string{"ctx-size": "8192"}, stored())

	store(map[string]string{"ctx_size": "8192"})
	code, body = patchParams(t, s, map[string]any{"defaults": map[string]any{"parameters": map[string]any{"ctx-size": 4096}}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, map[string]string{"ctx-size": "4096"}, stored())
	assert.Equal(t, http.StatusOK, preview())

	store(map[string]string{"ctx_size": "1", "ctx-size": "2"})
	assert.Equal(t, []string{"ctx_size"}, resolvedFor(t, s, "llamacpp", qwenModel).Parameters["ctx-size"].Conflicts, "the view shows what a launch refuses")
	assert.Equal(t, http.StatusBadRequest, preview())
	code, body = patchParams(t, s, map[string]any{"defaults": map[string]any{"parameters": map[string]any{"ctx-size": nil}}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, stored())

	resp := makeAuthRequest(t, s, http.MethodPatch, apipath.ProviderParameters("llamacpp"), TestAdminKey, json.RawMessage(`{"defaults":{"parameters":{"ctx_size":1024,"ctx-size":2048}}}`))
	require.Equal(t, http.StatusBadRequest, resp.Code, string(resp.Body))
	problem := runParameterProblem(t, resp.Body)
	require.Len(t, problem.Errors, 2)
	for i, key := range []string{"ctx-size", "ctx_size"} {
		assert.Equal(t, key, problem.Errors[i].Key)
		assert.Equal(t, string(httperr.CodeDuplicateFlag), problem.Errors[i].Code)
	}
	assert.Equal(t, 1, strings.Count(problem.Detail, "spell one flag"), "the detail states it once: %s", problem.Detail)

	resp = makeAuthRequest(t, s, http.MethodPatch, apipath.ProviderParameters("llamacpp"), TestAdminKey, json.RawMessage(`{"defaults":{"parameters":{"ctx_size":1000,"ctx-size":1e3}}}`))
	require.Equal(t, http.StatusOK, resp.Code, "spellings that store one value agree: %s", resp.Body)
	assert.Equal(t, map[string]string{"ctx-size": "1000"}, stored())
}

// zz_params_get accepts names, on each parameter a runtime lacks, the
// runtimes that refuse it with excluded_by_runtime and the feature that
// selects each, as one runtime serves several features.
func TestSchemaNamesTheRuntimesThatExcludeAParameter(t *testing.T) {
	s, _ := assetTestNode(t)
	w := assetRequest(t, s, http.MethodGet, apipath.ProviderSchema("mlx"), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var declared schemaResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &declared))
	vlm := func(features ...string) []exclusionDTO {
		var out []exclusionDTO
		for _, feature := range features {
			out = append(out, exclusionDTO{Runtime: "mlx-vlm", Feature: feature})
		}
		return out
	}
	assert.Equal(t, vlm("embeddings", "reranking", "vision"), declared.Parameters["temp"].ExcludedBy)
	assert.Equal(t, vlm("embeddings", "reranking"), declared.Parameters["max-tokens"].ExcludedBy, "mlx_vlm serving a vision model takes max-tokens")
	assert.Empty(t, declared.Parameters["trust-remote-code"].ExcludedBy)
	require.Contains(t, declared.Parameters, "trust-remote-code")
	var wire struct {
		Parameters map[string]struct {
			ExcludedBy []map[string]string `json:"excluded_by"`
		} `json:"parameters"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &wire))
	svc, _ := s.appsConfig().LookupApp("mlx")
	assert.Equal(t, []map[string]string{{"runtime": svc.Name}}, wire.Parameters["kv-bits"].ExcludedBy, "the base runtime is named as the provider, with no feature")
}

// A served daemon owning its model settings is the remedy for a key it
// cannot be passed, not for a run setting refused for its value.
func TestServedRefusalHintsOnlyUnknownFlags(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", config.ServiceConfig{Enabled: new(true), Name: "ollama", Protocol: config.ProtocolOllama, Mode: "external", Runtime: &config.AppRuntimeConfig{Endpoint: "http://backend:11434"}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
	mgr, err := prov_apps.NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	_, err = NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "node" }).PreviewLocalRun(t.Context(), &PreviewRunRequest{ModelName: "local", Provider: "ollama", Parameters: map[string]string{"keep-alive": "soon", "temperature": "0.7"}})
	c, rec := testGinContext(http.MethodPost, "/zzrouter/v1/runs/preview")
	RespondToError(c, err)
	hints := map[string]string{}
	for _, issue := range runParameterProblem(t, rec.Body.Bytes()).Errors {
		hints[issue.Key] = issue.Hint
	}
	require.Len(t, hints, 2, rec.Body.String())
	assert.Contains(t, hints["temperature"], "daemon owns its model settings")
	assert.Contains(t, hints["keep-alive"], "zz_params_get")
	assert.NotContains(t, hints["keep-alive"], "daemon owns")
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

// A bool llama.cpp also spells --no-<flag> is one key: the schema says so,
// false passes the --no- form, and the --no- key is refused with the key
// and value that pass it.
func TestNegatableBoolIsOneKey(t *testing.T) {
	s := variantNode(t)
	w := assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/llamacpp/schema", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var doc struct {
		Parameters map[string]map[string]any `json:"parameters"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &doc))
	assert.Equal(t, true, doc.Parameters["mmap"]["negatable"])
	assert.NotContains(t, doc.Parameters["no-host"], "negatable", "llama.cpp spells it only --no-host")
	assert.NotContains(t, doc.Parameters, "no-mmap")

	for value, flag := range map[string]string{"true": "--mmap", "false": "--no-mmap"} {
		w := assetRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", []byte(`{"model":"`+qwenModel+`","provider":"llamacpp","parameters":{"mmap":`+value+`}}`))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var preview struct {
			Data struct {
				FullCommand string `json:"full_command"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &preview))
		assert.Contains(t, strings.Fields(preview.Data.FullCommand), flag, value)
	}

	w = assetRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", []byte(`{"model":"`+qwenModel+`","provider":"llamacpp","parameters":{"no-mmap":true}}`))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	problem := runParameterProblem(t, w.Body.Bytes())
	require.Len(t, problem.Errors, 1)
	assert.Equal(t, string(httperr.CodeUnknownFlag), problem.Errors[0].Code)
	assert.Contains(t, problem.Errors[0].Message, "set mmap: false")
	code, body := patchParams(t, s, map[string]any{"defaults": map[string]any{"parameters": map[string]any{"no-mmap": true}}})
	require.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, "set mmap: false")
}
