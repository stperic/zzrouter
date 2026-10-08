package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// validatePatch happy path + every closed-enum rejection code exercised.
// Keeps the §7.2 wire contract pinned — plan §12 step-9 expects these
// shapes to reach agents unchanged.

func vllmSchemaForTest() *schema.ProviderSchema {
	s := make(map[string]schema.ParamShape, len(schema.Registry["vllm"]))
	for k, v := range schema.Registry["vllm"] {
		s[k] = v
	}
	return &schema.ProviderSchema{Parameters: s}
}

func ptrSpec(params, env map[string]json.RawMessage) *specPatch {
	return &specPatch{Parameters: params, Environment: env}
}

func ptrModel(params, env map[string]json.RawMessage) *modelPatch {
	return &modelPatch{specPatch: *ptrSpec(params, env)}
}

func raw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func emptyCfgWithModel(model string) *pkgConfig.ServiceConfig {
	return &pkgConfig.ServiceConfig{
		Models: map[string]pkgConfig.ModelSpec{model: {}},
	}
}

func TestValidatePatch_OK(t *testing.T) {
	cfg := emptyCfgWithModel("llama-3-70b")
	patch := &paramPatchBody{
		Models: map[string]*modelPatch{
			"llama-3-70b": ptrModel(map[string]json.RawMessage{
				"max-model-len": raw(32768),
			}, nil),
		},
	}
	nodes := map[string]bool{"worker-1": true}
	if errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: nodes}); len(errs) > 0 {
		t.Fatalf("expected nil, got %+v", errs)
	}
}

func TestValidatePatch_UnknownFlag(t *testing.T) {
	cfg := emptyCfgWithModel("llama-3-70b")
	patch := &paramPatchBody{
		Defaults: ptrDefaults(map[string]json.RawMessage{
			"ghost-flag": raw(1),
		}, nil),
	}
	errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: map[string]bool{}})
	if len(errs) == 0 || errs[0].Code != string(httperr.CodeUnknownFlag) {
		t.Fatalf("expected unknown_flag, got %+v", errs)
	}
}

func TestValidatePatch_WrongType(t *testing.T) {
	cfg := emptyCfgWithModel("llama-3-70b")
	patch := &paramPatchBody{
		Defaults: ptrDefaults(map[string]json.RawMessage{
			"max-model-len": raw("not-a-number"),
		}, nil),
	}
	errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: map[string]bool{}})
	if len(errs) == 0 || errs[0].Code != string(httperr.CodeWrongType) {
		t.Fatalf("expected wrong_type, got %+v", errs)
	}
}

func TestValidatePatch_OutOfRange(t *testing.T) {
	cfg := emptyCfgWithModel("llama-3-70b")
	patch := &paramPatchBody{
		Defaults: ptrDefaults(map[string]json.RawMessage{
			"gpu-memory-utilization": raw(2.5),
		}, nil),
	}
	errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: map[string]bool{}})
	if len(errs) == 0 || errs[0].Code != string(httperr.CodeOutOfRange) {
		t.Fatalf("expected out_of_range, got %+v", errs)
	}
}

func TestValidatePatch_UnknownNode(t *testing.T) {
	cfg := emptyCfgWithModel("llama-3-70b")
	patch := &paramPatchBody{
		Nodes: map[string]*nodePatch{
			"ghost": {Parameters: map[string]json.RawMessage{"max-model-len": raw(4096)}},
		},
	}
	errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: map[string]bool{"worker-1": true}})
	if len(errs) == 0 || errs[0].Code != string(httperr.CodeUnknownNode) {
		t.Fatalf("expected unknown_node, got %+v", errs)
	}
}

func TestValidatePatch_UnknownModelOnlyAtNodeCell(t *testing.T) {
	// Tier 1 models.X is a declaration; first write must pass. The
	// orphan-cell guard fires only when a (node, model) cell references
	// a model that exists nowhere in the tree or this patch.
	cfg := emptyCfgWithModel("llama-3-70b")
	patch := &paramPatchBody{
		Nodes: map[string]*nodePatch{
			"worker-1": {
				Models: map[string]*specPatch{
					"phantom-model": ptrSpec(map[string]json.RawMessage{
						"max-model-len": raw(4096),
					}, nil),
				},
			},
		},
	}
	errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: map[string]bool{"worker-1": true}})
	if len(errs) == 0 || errs[0].Code != string(httperr.CodeUnknownModel) {
		t.Fatalf("expected unknown_model, got %+v", errs)
	}
}

func TestValidatePatch_Tier1DeclarationAccepted(t *testing.T) {
	// Empty tree + Tier-1 declaration must succeed — otherwise there's
	// no bootstrap path.
	cfg := &pkgConfig.ServiceConfig{}
	patch := &paramPatchBody{
		Models: map[string]*modelPatch{
			"llama3": ptrModel(map[string]json.RawMessage{"max-model-len": raw(4096)}, nil),
		},
	}
	if errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: map[string]bool{}}); len(errs) > 0 {
		t.Fatalf("Tier-1 declaration must pass, got %+v", errs)
	}
}

func TestValidatePatch_AtomicDeclareAndPinCell(t *testing.T) {
	// models.X + nodes.N.models.X in one PATCH — the Tier-1 declaration
	// promotes the model to knownModels for the Tier-3 check that runs
	// after in the same pass.
	cfg := &pkgConfig.ServiceConfig{}
	patch := &paramPatchBody{
		Models: map[string]*modelPatch{
			"llama3": ptrModel(map[string]json.RawMessage{"max-model-len": raw(2048)}, nil),
		},
		Nodes: map[string]*nodePatch{
			"worker-1": {
				Models: map[string]*specPatch{
					"llama3": ptrSpec(map[string]json.RawMessage{"max-model-len": raw(8192)}, nil),
				},
			},
		},
	}
	if errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: map[string]bool{"worker-1": true}}); len(errs) > 0 {
		t.Fatalf("atomic declare + pin must pass, got %+v", errs)
	}
}

func TestValidatePatch_NullIsValid(t *testing.T) {
	cfg := emptyCfgWithModel("llama-3-70b")
	patch := &paramPatchBody{
		Defaults: ptrDefaults(map[string]json.RawMessage{
			"max-model-len": json.RawMessage("null"),
		}, nil),
	}
	if errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: map[string]bool{}}); len(errs) > 0 {
		t.Fatalf("null must be valid (delete-if-present), got %+v", errs)
	}
}

// TestValidatePatch_EndpointScopedEnumRejected pins endpoint-scoped enum
// enforcement: a string param declared with an enum under
// endpoints.<E>.parameters must reject out-of-set values when the patch
// targets that endpoint scope.
func TestValidatePatch_EndpointScopedEnumRejected(t *testing.T) {
	cfg := &pkgConfig.ServiceConfig{}
	sch := &schema.ProviderSchema{
		Parameters: map[string]schema.ParamShape{},
		Endpoints: map[string]schema.EndpointSchema{
			"embeddings": {
				Parameters: map[string]schema.ParamShape{
					"pooling": {Kind: schema.ParamString, Enum: []string{"none", "mean", "cls"}},
				},
			},
		},
	}
	patch := &paramPatchBody{
		Defaults: &defaultsPatch{
			Endpoints: map[string]*endpointPatch{
				"embeddings": {
					Parameters: map[string]json.RawMessage{
						"pooling": raw("frobnicate"),
					},
				},
			},
		},
	}
	errs := validatePatch("llamacpp", cfg, patch, sch, patchScope{nodes: map[string]bool{}})
	if len(errs) == 0 || errs[0].Code != string(httperr.CodeCoercionFailed) {
		t.Fatalf("expected enum rejection, got %+v", errs)
	}

	// Same patch with an allowed value passes.
	patch.Defaults.Endpoints["embeddings"].Parameters["pooling"] = raw("mean")
	if errs := validatePatch("llamacpp", cfg, patch, sch, patchScope{nodes: map[string]bool{}}); len(errs) > 0 {
		t.Fatalf("allowed enum value must pass, got %+v", errs)
	}
}

func TestValidatePatch_EnvValidatesPreApply(t *testing.T) {
	cfg := emptyCfgWithModel("llama-3-70b")
	patch := &paramPatchBody{
		Defaults: ptrDefaults(nil, map[string]json.RawMessage{
			"lower-case-not-allowed": raw("x"),
		}),
	}
	errs := validatePatch("vllm", cfg, patch, vllmSchemaForTest(), patchScope{nodes: map[string]bool{}})
	if len(errs) == 0 || errs[0].Code != string(httperr.CodeCoercionFailed) {
		t.Fatalf("env key validation must run in validatePatch (H4), got %+v", errs)
	}
}

// TestApplyPatch_EndpointsAddAndDelete pins the endpoint-overlay PATCH
// path: setting endpoints.<name>.parameters.<k>=<v> creates the overlay
// (or merges into an existing one), and setting endpoints.<name>=null
// deletes the whole overlay. Closes the operator-ergonomic gap from the
// endpoint-aware launch arc — operators no longer have to direct-edit
// YAML to add or remove a per-endpoint overlay.
func TestApplyPatch_EndpointsAddAndDelete(t *testing.T) {
	sc := &pkgConfig.ServiceConfig{
		Defaults: &pkgConfig.AppDefaultsConfig{},
	}

	// Step 1 — add an embeddings overlay via PATCH.
	addPatch := &paramPatchBody{
		Defaults: &defaultsPatch{
			Endpoints: map[string]*endpointPatch{
				"embeddings": {
					Parameters: map[string]json.RawMessage{
						"embeddings": raw("true"),
						"pooling":    raw("mean"),
					},
				},
			},
		},
	}
	if err := applyPatchToConfig(sc, addPatch); err != nil {
		t.Fatalf("apply add patch: %v", err)
	}
	overlay, ok := sc.Defaults.Endpoints["embeddings"]
	if !ok {
		t.Fatal("endpoints.embeddings overlay missing after add patch")
	}
	if overlay.Parameters["embeddings"] != "true" || overlay.Parameters["pooling"] != "mean" {
		t.Errorf("overlay parameters not merged: %+v", overlay.Parameters)
	}

	// Step 2 — second PATCH adds a key without losing the first.
	mergePatch := &paramPatchBody{
		Defaults: &defaultsPatch{
			Endpoints: map[string]*endpointPatch{
				"embeddings": {
					Parameters: map[string]json.RawMessage{"verbose": raw("true")},
				},
			},
		},
	}
	if err := applyPatchToConfig(sc, mergePatch); err != nil {
		t.Fatalf("apply merge patch: %v", err)
	}
	overlay = sc.Defaults.Endpoints["embeddings"]
	if overlay.Parameters["embeddings"] != "true" || overlay.Parameters["pooling"] != "mean" || overlay.Parameters["verbose"] != "true" {
		t.Errorf("merge dropped existing keys: %+v", overlay.Parameters)
	}

	// Step 3 — null at endpoints.<name> deletes the whole overlay.
	deletePatch := &paramPatchBody{
		Defaults: &defaultsPatch{
			Endpoints: map[string]*endpointPatch{
				"embeddings": nil,
			},
		},
	}
	if err := applyPatchToConfig(sc, deletePatch); err != nil {
		t.Fatalf("apply delete patch: %v", err)
	}
	if _, present := sc.Defaults.Endpoints["embeddings"]; present {
		t.Errorf("endpoints.embeddings overlay still present after null patch: %+v", sc.Defaults.Endpoints)
	}

	// Step 4 — null on a missing overlay is a clean no-op (idempotent).
	if err := applyPatchToConfig(sc, deletePatch); err != nil {
		t.Fatalf("idempotent null delete: %v", err)
	}

	// Step 5 — leaf-level null inside an overlay deletes a single key,
	// and the now-empty overlay collapses (parameters+env both empty).
	sc.Defaults.Endpoints = map[string]pkgConfig.EndpointOverlay{
		"reranking": {Parameters: map[string]string{"reranking": "true"}},
	}
	leafNullPatch := &paramPatchBody{
		Defaults: &defaultsPatch{
			Endpoints: map[string]*endpointPatch{
				"reranking": {
					Parameters: map[string]json.RawMessage{"reranking": raw(nil)},
				},
			},
		},
	}
	if err := applyPatchToConfig(sc, leafNullPatch); err != nil {
		t.Fatalf("leaf-null patch: %v", err)
	}
	if _, present := sc.Defaults.Endpoints["reranking"]; present {
		t.Errorf("emptied overlay must collapse: %+v", sc.Defaults.Endpoints)
	}
}

func TestValidatePatch_AssetParameter(t *testing.T) {
	sch := &schema.ProviderSchema{
		Parameters: map[string]schema.ParamShape{"chat-template-file": {Kind: schema.ParamAsset}},
		Endpoints: map[string]schema.EndpointSchema{
			"embeddings": {Parameters: map[string]schema.ParamShape{"pooling-file": {Kind: schema.ParamAsset}}},
		},
	}
	codeOf := func(v any) string {
		t.Helper()
		errs := validatePatch("llamacpp", emptyCfgWithModel("m"), &paramPatchBody{
			Defaults: ptrDefaults(map[string]json.RawMessage{"chat-template-file": raw(v)}, nil),
		}, sch, patchScope{nodes: map[string]bool{}})
		if len(errs) == 0 {
			return ""
		}
		require.Len(t, errs, 1)
		return errs[0].Code
	}

	assert.Equal(t, "", codeOf("qwen.jinja"), "existence is checked when the patch is applied")
	assert.Equal(t, string(httperr.CodeInvalidValue), codeOf("../etc/passwd"))
	assert.Equal(t, string(httperr.CodeInvalidValue), codeOf("/abs/path.jinja"))
	assert.Equal(t, string(httperr.CodeWrongType), codeOf(7))

	nulled := validatePatch("llamacpp", emptyCfgWithModel("m"), &paramPatchBody{
		Defaults: ptrDefaults(map[string]json.RawMessage{"chat-template-file": json.RawMessage("null")}, nil),
	}, sch, patchScope{nodes: map[string]bool{}})
	assert.Empty(t, nulled, "null deletes, as for every key")

	overlay := validatePatch("llamacpp", emptyCfgWithModel("m"), &paramPatchBody{
		Defaults: &defaultsPatch{Endpoints: map[string]*endpointPatch{
			"embeddings": {Parameters: map[string]json.RawMessage{"pooling-file": raw("missing.bin")}},
		}},
	}, sch, patchScope{nodes: map[string]bool{}})
	assert.Empty(t, overlay)

	bad := validatePatch("llamacpp", emptyCfgWithModel("m"), &paramPatchBody{
		Defaults: &defaultsPatch{Endpoints: map[string]*endpointPatch{
			"embeddings": {Parameters: map[string]json.RawMessage{"pooling-file": raw("../x")}},
		}},
	}, sch, patchScope{nodes: map[string]bool{}})
	require.Len(t, bad, 1, "an asset key declared only in an endpoint overlay is still checked")
	assert.Equal(t, string(httperr.CodeInvalidValue), bad[0].Code)
}

// A kind the validator does not handle must refuse, not wave the value
// through: that is how an asset-typed key would pass unchecked had its
// case been forgotten.
func TestValidateAgainstShape_UnknownKindRefuses(t *testing.T) {
	e := validateAgainstShape("k", raw("anything"), schema.ParamShape{Kind: "mystery"})
	require.NotNil(t, e)
	assert.Equal(t, string(httperr.CodeWrongType), e.Code)
}

// The PATCH handler asks the coordinator's own asset directory, through
// the store, whether a named asset exists.
func TestParametersPatch_ChecksAssetOnCoordinator(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)

	providerDir := filepath.Join(server.configStore.DirPath(), "on-demand", "llamacpp")
	require.NoError(t, os.WriteFile(filepath.Join(providerDir, "schema.yaml"),
		[]byte("parameters:\n  chat-template-file: {type: asset}\n"), 0o600))
	dir, err := server.configStore.Assets("llamacpp")
	require.NoError(t, err)
	_, err = dir.Write("present.jinja", []byte("{{ messages }}"))
	require.NoError(t, err)

	patch := func(name string) *TestResponse {
		return makeAuthRequest(t, server, "PATCH", "/zzrouter/v1/providers/llamacpp/parameters", TestAdminKey,
			map[string]any{"defaults": map[string]any{"parameters": map[string]any{"chat-template-file": name}}})
	}

	resp := patch("present.jinja")
	assert.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

	resp = patch("absent.jinja")
	assert.Equal(t, http.StatusBadRequest, resp.Code)
	assert.Contains(t, string(resp.Body), `"code":"unknown_asset"`)
}

// A reference that was already dangling (its file removed by hand) is not
// a PATCH's doing: unrelated writes still land, while a write that makes
// a new reference dangle is refused.
func TestParametersPatch_RefusesOnlyNewDanglingRefs(t *testing.T) {
	server, _ := assetTestNode(t)
	require.NoError(t, server.configStore.SetAppParameter("llamacpp", "chat-template-file", "t.jinja"))
	require.NoError(t, os.Remove(filepath.Join(server.configStore.DirPath(), "on-demand", "llamacpp", "assets", "t.jinja")))

	patch := func(body map[string]any) *TestResponse {
		return makeAuthRequest(t, server, "PATCH", "/zzrouter/v1/providers/llamacpp/parameters", TestAdminKey, body)
	}
	resp := patch(map[string]any{"defaults": map[string]any{"parameters": map[string]any{"ctx-size": 4096}}})
	assert.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

	resp = patch(map[string]any{"models": map[string]any{"m": map[string]any{"parameters": map[string]any{"chat-template-file": "t.jinja"}}}})
	assert.Equal(t, http.StatusBadRequest, resp.Code)
	assert.Contains(t, string(resp.Body), `"key":"models.m.parameters.chat-template-file"`)
	assert.Contains(t, string(resp.Body), `"code":"unknown_asset"`)
}

// Every site a parameter can be set at is held to the same rule, and the
// refusal names the site.
func TestParametersPatch_RefusesDanglingAssetAtEverySite(t *testing.T) {
	server, _ := assetTestNode(t)
	node := server.node.Nodename()
	params := map[string]any{"parameters": map[string]any{"chat-template-file": "absent.jinja"}}

	cases := map[string]map[string]any{
		"nodes." + node + ".parameters.chat-template-file": {
			"nodes": map[string]any{node: params}},
		"nodes." + node + ".models.m.parameters.chat-template-file": {
			"models": map[string]any{"m": map[string]any{"parameters": map[string]any{"ctx-size": 4096}}},
			"nodes":  map[string]any{node: map[string]any{"models": map[string]any{"m": params}}}},
		"defaults.endpoints.embeddings.parameters.chat-template-file": {
			"defaults": map[string]any{"endpoints": map[string]any{"embeddings": params}}},
		"models.m.endpoints.embeddings.parameters.chat-template-file": {
			"models": map[string]any{"m": map[string]any{"endpoints": map[string]any{"embeddings": params}}}},
	}
	for key, body := range cases {
		t.Run(key, func(t *testing.T) {
			resp := makeAuthRequest(t, server, "PATCH", "/zzrouter/v1/providers/llamacpp/parameters", TestAdminKey, body)
			assert.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body)
			assert.Contains(t, string(resp.Body), `"key":"`+key+`"`)
			assert.Contains(t, string(resp.Body), `"code":"unknown_asset"`)
		})
	}
}

// A PATCH setting only an endpoint overlay on a model or node is kept,
// not answered 200 and dropped.
func TestParametersPatch_KeepsOverlayOnlyTiers(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)
	node := server.node.Nodename()
	overlay := map[string]any{"endpoints": map[string]any{"embeddings": map[string]any{"parameters": map[string]any{"pooling": "mean"}}}}
	body := map[string]any{
		"models": map[string]any{"m": overlay},
		"nodes":  map[string]any{node: overlay},
	}
	resp := makeAuthRequest(t, server, "PATCH", "/zzrouter/v1/providers/llamacpp/parameters", TestAdminKey, body)
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

	sc, ok := server.configStore.Config().LookupApp("llamacpp")
	require.True(t, ok)
	assert.Equal(t, "mean", sc.Models["m"].Endpoints["embeddings"].Parameters["pooling"])
	assert.Equal(t, "mean", sc.Nodes[node].Endpoints["embeddings"].Parameters["pooling"])
}

// A write reports what it means for the running models, and a restart
// value it does not know is refused before anything is written.
func TestParametersPatch_ReportsRunsAndChecksRestart(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)
	path := "/zzrouter/v1/providers/llamacpp/parameters"
	body := map[string]any{"defaults": map[string]any{"parameters": map[string]any{"ctx-size": 4096}}}

	ctxSize := func() any {
		svc, ok := server.configStore.Config().LookupApp("llamacpp")
		require.True(t, ok)
		return svc.Resolve("", "").Parameters["ctx-size"].Value
	}
	before := ctxSize()
	require.NotEqual(t, "4096", before)

	resp := makeAuthRequest(t, server, "PATCH", path+"?restart=all", TestAdminKey, body)
	assert.Equal(t, http.StatusBadRequest, resp.Code)
	assert.Contains(t, string(resp.Body), `"key":"restart"`)
	assert.Equal(t, before, ctxSize(), "a refused restart value wrote nothing")

	resp = makeAuthRequest(t, server, "PATCH", path+"?restart=affected", TestAdminKey, body)
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)
	var got struct {
		Parameters map[string]any `json:"parameters"`
		Runs       *RunsReport    `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &got))
	assert.Contains(t, got.Parameters, "ctx-size", "the resolved view is still the body")
	require.NotNil(t, got.Runs)
	assert.Empty(t, got.Runs.Stale, "nothing is running on a fresh node")
	assert.Empty(t, got.Runs.RestartJobID)
}

func ptrDefaults(parameters, environment map[string]json.RawMessage) *defaultsPatch {
	return &defaultsPatch{Parameters: parameters, Environment: environment}
}
