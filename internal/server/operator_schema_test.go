package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func operatorExecutor(t *testing.T, onDemand ...bool) (*ParamsExecutor, *gin.Engine) {
	t.Helper()
	store := newFanOutTestStore(t)
	if len(onDemand) > 0 && onDemand[0] {
		root := t.TempDir()
		dir := filepath.Join(root, "on-demand", "ollama")
		require.NoError(t, os.MkdirAll(dir, 0700))
		body := "protocol: openai\nenabled: true\nruntime: {port_range: [8188, 8188], base_port: 8188, execution: {type: cli, command: engine}}\ncapabilities: {wire_endpoints: [chat_completions, embeddings]}\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0600))
		var err error
		store, err = config.NewAppsConfigStore(root)
		require.NoError(t, err)
	}
	e := NewParamsExecutor(store.Config, store, func() string { return "local" }, nil, nil, nil, nil, nil)
	return e, operatorRoutes(e)
}

func operatorRoutes(e *ParamsExecutor) *gin.Engine {
	r := gin.New()
	r.PATCH("/providers/:name/parameters", e.HandleParametersPatch)
	r.GET("/providers/:name/schema", e.HandleSchema)
	return r
}

func operatorRequest(r http.Handler, method, body string) *httptest.ResponseRecorder {
	path := "/providers/ollama/parameters"
	if method == http.MethodGet {
		path = "/providers/ollama/schema"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func operatorPatch(t *testing.T, r http.Handler, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	rec := operatorRequest(r, http.MethodPatch, body)
	require.Equal(t, status, rec.Code, rec.Body.String())
	return rec
}

func TestOperatorSchemaPatchDeclareAndSet(t *testing.T) {
	e, r := operatorExecutor(t)
	operatorPatch(t, r, `{"schema":{"future_flag":{"type":"int","min":1,"max":10}},"defaults":{"parameters":{"future-flag":5}}}`, 200)
	cfg, ok := e.configStore.Config().LookupApp("ollama")
	require.True(t, ok)
	require.Equal(t, "5", cfg.Defaults.Parameters["future-flag"])
	require.Equal(t, "int", *cfg.Schema["future_flag"].Type)
	copy, err := cloneService(&cfg)
	require.NoError(t, err)
	*copy.Schema["future_flag"].Max = 99
	assert.Equal(t, 10.0, *cfg.Schema["future_flag"].Max)
	view := operatorRequest(r, http.MethodGet, "")
	require.Equal(t, 200, view.Code)
	var response schemaResponse
	require.NoError(t, json.Unmarshal(view.Body.Bytes(), &response))
	assert.True(t, response.Parameters["future-flag"].Operator)
	assert.Empty(t, response.Problems)
	// Partial field patches merge, and null deletes a field without dropping its siblings.
	operatorPatch(t, r, `{"schema":{"future_flag":{"description":"updated","min":null}}}`, 200)
	cfg, _ = e.configStore.Config().LookupApp("ollama")
	assert.Nil(t, cfg.Schema["future_flag"].Min)
	assert.Equal(t, "int", *cfg.Schema["future_flag"].Type)
	assert.Equal(t, "updated", *cfg.Schema["future_flag"].Description)
	assert.Equal(t, 10.0, *cfg.Schema["future_flag"].Max)
}

func TestOperatorSchemaPatchRefusesAtomically(t *testing.T) {
	e, r := operatorExecutor(t)
	for _, tc := range []struct{ name, body, key, code string }{
		{"undeclared", `{"defaults":{"parameters":{"future":1}}}`, "future", "unknown_flag"},
		{"bad declaration", `{"schema":{"future":{"type":"invented"}},"defaults":{"parameters":{"future":1},"environment":{"ZZ_FUTURE":"must not land"}}}`, "schema.future", "invalid_value"},
		{"invalid key", `{"schema":{"bad key --x=1":{"type":"int"}},"defaults":{"environment":{"ZZ_FUTURE":"must not land"}}}`, "schema.bad key --x=1", "invalid_value"},
		{"leading hyphen", `{"schema":{"-lead":{"type":"bool"}},"defaults":{"environment":{"ZZ_FUTURE":"must not land"}}}`, "schema.-lead", "invalid_value"},
		{"reversed range", `{"schema":{"future":{"type":"int","min":5,"max":1}},"defaults":{"environment":{"ZZ_FUTURE":"must not land"}}}`, "schema.future.min", "invalid_value"},
		{"served field", `{"schema":{"future":{"type":"string","served":"secret"}},"defaults":{"parameters":{"future":"value"}}}`, "served", "unknown_field"},
		{"asset", `{"schema":{"future":{"type":"asset"}},"defaults":{"parameters":{"future":"missing.txt"}}}`, "defaults.parameters.future", "unknown_asset"},
		{"owned", `{"schema":{"keep-alive":{"description":"owned"}},"defaults":{"environment":{"ZZ_FUTURE":"must not land"}}}`, "schema.keep-alive", "invalid_value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := e.configStore.ReadProviderConfigBytes("ollama")
			require.NoError(t, err)
			rec := operatorPatch(t, r, tc.body, 400)
			var problem utils.ProblemDetails
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
			require.NotEmpty(t, problem.Errors)
			found := false
			for _, p := range problem.Errors {
				if p.Key == tc.key && p.Code == tc.code {
					found = true
				}
			}
			assert.True(t, found, rec.Body.String())
			after, err := e.configStore.ReadProviderConfigBytes("ollama")
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestOperatorSchemaEndpointOnlyPatch(t *testing.T) {
	e, r := operatorExecutor(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(e.configStore.DirPath(), "on-demand", "ollama", "schema.yaml"), []byte("endpoints:\n  embeddings:\n    parameters:\n      endpoint_only: {type: int, enum: ['2']}\n  reranking:\n    parameters:\n      endpoint-only: {type: int, enum: ['3']}\n"), 0600))
	before, err := e.configStore.ReadProviderConfigBytes("ollama")
	require.NoError(t, err)
	rec := operatorPatch(t, r, `{"schema":{"endpoint-only":{"type":"int","enum":["1","2"]}},"defaults":{"environment":{"ZZ_FUTURE":"must not land"}}}`, 400)
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	require.Len(t, problem.Errors, 1)
	assert.Equal(t, "schema.endpoint-only", problem.Errors[0].Key)
	assert.Contains(t, problem.Errors[0].Message, "embeddings")
	assert.Contains(t, problem.Errors[0].Message, "reranking")
	after, err := e.configStore.ReadProviderConfigBytes("ollama")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	operatorPatch(t, r, `{"defaults":{"endpoints":{"embeddings":{"parameters":{"endpoint-only":2}}}}}`, 200)
	operatorPatch(t, r, `{"defaults":{"endpoints":{"embeddings":{"parameters":{"endpoint-only":1}}}}}`, 400)
	operatorPatch(t, r, `{"defaults":{"parameters":{"endpoint-only":2}}}`, 400)
}

type operatorWarningHandler struct {
	slog.Handler
	warnings atomic.Int32
}

func (h *operatorWarningHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "operator schema field dropped" {
		h.warnings.Add(1)
	}
	return nil
}

func TestOperatorSchemaLogsOnlyCommittedBuild(t *testing.T) {
	e, r := operatorExecutor(t)
	require.NoError(t, e.configStore.ApplyParameterPatch("ollama", func(sc *config.ServiceConfig) error {
		sc.Schema = map[string]config.ParameterDeclaration{"bad": {Type: new("invented")}}
		return nil
	}))
	handler := &operatorWarningHandler{Handler: slog.NewTextHandler(io.Discard, nil)}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	cfg, _ := e.configStore.Config().LookupApp("ollama")
	merged, errs := serviceSchema("ollama", cfg, &schema.YAMLSchema{Parameters: map[string]schema.YAMLParam{"invalid": {Type: "int", Min: new(5.0), Max: new(1.0)}}})
	require.Len(t, errs, 1)
	require.Len(t, merged.Problems, 1)
	assert.Zero(t, handler.warnings.Load())
	merged, err := providerSchema(e.configStore)("ollama")
	require.NoError(t, err)
	require.Len(t, merged.Problems, 1)
	assert.EqualValues(t, 1, handler.warnings.Load())
	operatorPatch(t, r, `{"defaults":{"environment":{"ZZ_UNRELATED":"allowed"}}}`, 200)
	operatorPatch(t, r, `{"schema":{"hypothetical":{"type":"invented"}}}`, 400)
	assert.EqualValues(t, 1, handler.warnings.Load(), "PATCH before and after builds stay silent")
}

func TestModelCellPatchPreservesRequestIntegers(t *testing.T) {
	spec := config.ModelSpec{Request: map[string]any{"seed": int64(9007199254740993), "nested": map[string]any{"count": 7, "remove": "old"}}}
	require.NoError(t, applyModelCellPatch(&spec, &modelPatch{Request: json.RawMessage(`{"temperature":0.5,"nested":{"remove":null,"new":true}}`)}))
	assert.Equal(t, int64(9007199254740993), spec.Request["seed"])
	nested := spec.Request["nested"].(map[string]any)
	assert.Equal(t, 7, nested["count"])
	assert.NotContains(t, nested, "remove")
	assert.Equal(t, true, nested["new"])
	assert.Equal(t, 0.5, spec.Request["temperature"])
}

func TestOperatorSchemaPatchUsedDeclaration(t *testing.T) {
	for _, site := range []struct{ name, body, path string }{
		{"default", `"defaults":{"parameters":{"future":5}}`, "defaults.parameters.future"},
		{"model", `"models":{"m":{"parameters":{"future":5}}}`, "models.m.parameters.future"},
		{"node", `"nodes":{"local":{"parameters":{"future":5}}}`, "nodes.local.parameters.future"},
		{"cell endpoint", `"models":{"m":{}},"nodes":{"local":{"models":{"m":{"endpoints":{"embeddings":{"parameters":{"future":5}}}}}}}`, "nodes.local.models.m.endpoints.embeddings.parameters.future"},
	} {
		t.Run(site.name, func(t *testing.T) {
			e, r := operatorExecutor(t, true)
			operatorPatch(t, r, `{"schema":{"future":{"type":"int","max":10}},`+site.body+`}`, 200)
			for _, body := range []string{`{"schema":{"future":null}}`, `{"schema":{"future":{"max":3}}}`, `{"schema":null}`} {
				before, err := e.configStore.ReadProviderConfigBytes("ollama")
				require.NoError(t, err)
				rec := operatorPatch(t, r, body, 400)
				var problem utils.ProblemDetails
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
				require.NotEmpty(t, problem.Errors)
				assert.Equal(t, site.path, problem.Errors[0].Key)
				after, err := e.configStore.ReadProviderConfigBytes("ollama")
				require.NoError(t, err)
				assert.Equal(t, before, after)
			}
		})
	}
	e, r := operatorExecutor(t, true)
	operatorPatch(t, r, `{"schema":{"future":{"type":"int"}},"defaults":{"parameters":{"future":5}}}`, 200)
	operatorPatch(t, r, `{"schema":{"future":null},"defaults":{"parameters":{"future":null}}}`, 200)
	cfg, _ := e.configStore.Config().LookupApp("ollama")
	assert.Empty(t, cfg.Schema)
	assert.NotContains(t, cfg.Defaults.Parameters, "future")
}

func TestOperatorSchemaLoadProblemsAndPromotion(t *testing.T) {
	e, r := operatorExecutor(t)
	require.NoError(t, e.configStore.ApplyParameterPatch("ollama", func(sc *config.ServiceConfig) error {
		sc.Schema = map[string]config.ParameterDeclaration{
			"bad":    {Type: new("invented")},
			"future": {Type: new("int"), Min: new(1.0), Enum: new([]string{"2", "4"}), Auto: new("auto")},
		}
		return nil
	}))
	rec := operatorRequest(r, http.MethodGet, "")
	require.Equal(t, 200, rec.Code)
	var response schemaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Len(t, response.Problems, 1)
	assert.Equal(t, "schema.bad", response.Problems[0].Key)
	assert.NotContains(t, response.Parameters, "bad")
	assert.True(t, response.Parameters["future"].Operator)
	operatorPatch(t, r, `{"defaults":{"parameters":{"future":2}}}`, 200)
	require.NoError(t, os.WriteFile(filepath.Join(e.configStore.DirPath(), "external", "ollama", "schema.yaml"), []byte("parameters:\n  future: {type: int, min: 3, enum: ['3']}\n"), 0600))
	rec = operatorRequest(r, http.MethodGet, "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Len(t, response.Problems, 3)
	assert.Equal(t, []string{"2", "4"}, response.Parameters["future"].Enum)
	assert.Equal(t, 1.0, *response.Parameters["future"].Min)
	operatorPatch(t, r, `{"defaults":{"environment":{"ZZ_UNRELATED":"allowed"}}}`, 200)
	// A newly invalid field is still refused even when older release problems remain.
	operatorPatch(t, r, `{"schema":{"future":{"pass":"content"}}}`, 400)
}

func TestOperatorSchemaPatchWorker(t *testing.T) {
	e, _ := operatorExecutor(t)
	e.isWorker = func() bool { return true }
	e.coordURL = func() string { return "http://coordinator:9090" }
	rec := operatorPatch(t, operatorRoutes(e), `{"schema":{"future":{"type":"int"}},"defaults":{"parameters":{"future":1}}}`, 421)
	assert.Contains(t, rec.Header().Get("Location"), "coordinator:9090")
	cfg, _ := e.configStore.Config().LookupApp("ollama")
	assert.Empty(t, cfg.Schema)
}

func TestOperatorSchemaConcurrentNarrowAndValue(t *testing.T) {
	for _, first := range []string{"narrow", "value"} {
		t.Run(first, func(t *testing.T) {
			e, r := operatorExecutor(t)
			operatorPatch(t, r, `{"schema":{"future":{"type":"int","max":10}},"defaults":{"parameters":{"future":2}}}`, 200)
			ready := make(chan struct{}, 2)
			permits := map[string]chan struct{}{"narrow": make(chan struct{}), "value": make(chan struct{})}
			results := map[string]chan *httptest.ResponseRecorder{"narrow": make(chan *httptest.ResponseRecorder, 1), "value": make(chan *httptest.ResponseRecorder, 1)}
			for name, body := range map[string]string{"narrow": `{"schema":{"future":{"max":3}}}`, "value": `{"defaults":{"parameters":{"future":8}}}`} {
				copy := *e
				copy.knownNodes = func() map[string]bool {
					ready <- struct{}{}
					select {
					case <-permits[name]:
					case <-t.Context().Done():
					}
					return map[string]bool{"local": true}
				}
				route := operatorRoutes(&copy)
				go func() { results[name] <- operatorRequest(route, http.MethodPatch, body) }()
			}
			for range 2 {
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					t.Fatal("writes did not reach the pre-lock barrier")
				}
			}
			close(permits[first])
			var committed *httptest.ResponseRecorder
			select {
			case committed = <-results[first]:
			case <-time.After(5 * time.Second):
				t.Fatal("first write did not finish")
			}
			require.Equal(t, 200, committed.Code, committed.Body.String())
			second := "value"
			if first == "value" {
				second = "narrow"
			}
			close(permits[second])
			select {
			case refused := <-results[second]:
				require.Equal(t, 400, refused.Code, refused.Body.String())
				var problem utils.ProblemDetails
				require.NoError(t, json.Unmarshal(refused.Body.Bytes(), &problem))
				require.NotEmpty(t, problem.Errors)
				assert.Equal(t, "defaults.parameters.future", problem.Errors[0].Key)
			case <-time.After(5 * time.Second):
				t.Fatal("second write did not finish")
			}
			cfg, _ := e.configStore.Config().LookupApp("ollama")
			if first == "narrow" {
				assert.Equal(t, "2", cfg.Defaults.Parameters["future"])
				assert.Equal(t, 3.0, *cfg.Schema["future"].Max)
			} else {
				assert.Equal(t, "8", cfg.Defaults.Parameters["future"])
				assert.Equal(t, 10.0, *cfg.Schema["future"].Max)
			}
		})
	}
}

func TestOperatorSchemaLaunchArgv(t *testing.T) {
	root := t.TempDir()
	seedTestProviders(t, root, []string{"llamacpp"}, nil)
	store, err := config.NewAppsConfigStore(root)
	require.NoError(t, err)
	require.NoError(t, store.ApplyParameterPatch("llamacpp", func(sc *config.ServiceConfig) error {
		sc.Schema = map[string]config.ParameterDeclaration{
			"future_flag":     {Type: new("bool"), Negatable: new(true)},
			"future_template": {Type: new("asset"), Pass: new("content")},
			"future_mode":     {Type: new("string"), Auto: new("auto")},
		}
		return nil
	}))
	_, err = store.WriteAsset("llamacpp", "template.txt", []byte("operator content"))
	require.NoError(t, err)
	m := schemaLaunchManager(t, store, "llamacpp")
	out, err := m.PreviewLaunch(t.Context(), prov_apps.EnsureRequest{Provider: "llamacpp", Model: "m", Parameters: map[string]string{"future-flag": "false", "future-template": "template.txt", "future-mode": "auto"}}, 8100)
	require.NoError(t, err)
	assert.Contains(t, out.Args, "--no-future_flag")
	assert.Contains(t, out.Args, "--future_template")
	assert.Contains(t, out.Args, "operator content")
	assert.Contains(t, out.Args, "--future_mode")
	assert.Contains(t, out.Args, "auto")
}

func TestOperatorSchemaEmptyEnumOverridesShipped(t *testing.T) {
	e, r := operatorExecutor(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.configStore.DirPath(), "external", "ollama", "schema.yaml"), []byte("parameters:\n  future: {type: string, enum: [shipped]}\n"), 0600))
	operatorPatch(t, r, `{"schema":{"future":{"enum":[]}},"defaults":{"parameters":{"future":"widened"}}}`, 200)
	cfg, _ := e.configStore.Config().LookupApp("ollama")
	require.NotNil(t, cfg.Schema["future"].Enum)
	assert.Empty(t, *cfg.Schema["future"].Enum)
	reloaded, err := config.NewAppsConfigStore(e.configStore.DirPath())
	require.NoError(t, err)
	merged, err := providerSchema(reloaded)("ollama")
	require.NoError(t, err)
	assert.Empty(t, merged.Parameters["future"].Enum)
	assert.True(t, merged.Parameters["future"].Operator)
}
