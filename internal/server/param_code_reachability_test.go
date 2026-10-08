package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/httperr"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ParamErrorCode reachability.
//
// The enum is a published contract: agents branch on `code`, so each
// value is a promise that this exact string can come back. A member no
// test can produce is one of two things, and both are bugs worth
// knowing about — unreachable code that will never fire, or a real
// failure mode nobody has ever seen the wire shape of.
//
// So this file does not sample. Every declared code must have a case
// that provokes it through production code, and TestParamErrorCodes_
// AllAreReachable fails when a newly added constant has none. Adding a
// code to the enum therefore fails the build until someone shows it can
// actually be emitted.
//
// The cases deliberately go through the real producers — validatePatch,
// respondMutatorError, RetiredHandler — rather than constructing a
// ParamError literal. Asserting on a literal you wrote yourself proves
// only that Go assigns struct fields.

// paramCodeCase provokes one code and reports every code observed.
// Returning a slice rather than one value keeps a case honest when the
// producer emits several errors at once.
type paramCodeCase struct {
	// how names the trigger, so a failure says what stopped working
	// rather than only which code went missing.
	how     string
	provoke func(t *testing.T) []httperr.ParamErrorCode
}

func paramCodeCases() map[httperr.ParamErrorCode]paramCodeCase {
	return map[httperr.ParamErrorCode]paramCodeCase{
		httperr.CodeUnknownFlag: {
			how: "a parameter name the provider schema does not define",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromPatch(t, &paramPatchBody{
					Defaults: ptrDefaults(map[string]json.RawMessage{"ghost-flag": raw(1)}, nil),
				})
			},
		},
		httperr.CodeUnknownAsset: {
			how: "a parameter write naming an asset the provider does not have",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				t.Helper()
				server, _ := assetTestNode(t)
				c, rec := testGinContext(http.MethodPatch, "/zzrouter/v1/providers/llamacpp/parameters")
				c.Request = httptest.NewRequest(http.MethodPatch, "/zzrouter/v1/providers/llamacpp/parameters",
					strings.NewReader(`{"defaults":{"parameters":{"chat-template-file":"absent.jinja"}}}`))
				c.Request.Header.Set("Content-Type", "application/merge-patch+json")
				c.Params = gin.Params{{Key: "name", Value: "llamacpp"}}
				server.newParamsExecutor().HandleParametersPatch(c)
				return codesFromParamErrors(t, rec)
			},
		},
		httperr.CodeAssetInUse: {
			how: "deleting an asset a parameter still names",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				t.Helper()
				server, _ := assetTestNode(t)
				require.NoError(t, server.configStore.SetAppParameter("llamacpp", "chat-template-file", "t.jinja"))
				c, rec := testGinContext(http.MethodDelete, apipath.ProviderAsset("llamacpp", "t.jinja"))
				c.Params = gin.Params{{Key: "name", Value: "llamacpp"}, {Key: "asset", Value: "t.jinja"}}
				NewProviderAssetsController(server.configStore, templates.IsShippedAsset).Delete(c)
				return codesFromParamErrors(t, rec)
			},
		},
		httperr.CodeWrongType: {
			how: "a string sent for an integer-typed flag",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromPatch(t, &paramPatchBody{
					Defaults: ptrDefaults(map[string]json.RawMessage{"max-model-len": raw("not-a-number")}, nil),
				})
			},
		},
		httperr.CodeOutOfRange: {
			how: "a numeric flag outside its declared min/max",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromPatch(t, &paramPatchBody{
					Defaults: ptrDefaults(map[string]json.RawMessage{"gpu-memory-utilization": raw(9.5)}, nil),
				})
			},
		},
		httperr.CodeUnknownNode: {
			how: "a nodes.<name> block naming a node not in the cluster",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromPatch(t, &paramPatchBody{
					Nodes: map[string]*nodePatch{
						"no-such-node": {Parameters: map[string]json.RawMessage{"max-model-len": raw(4096)}},
					},
				})
			},
		},
		httperr.CodeUnknownModel: {
			how: "a nodes.<node>.models.<model> cell naming an unconfigured model",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromPatchWithNodes(t, map[string]bool{"node-a": true}, &paramPatchBody{
					Nodes: map[string]*nodePatch{
						"node-a": {Models: map[string]*specPatch{
							"no-such-model": ptrSpec(map[string]json.RawMessage{"max-model-len": raw(4096)}, nil),
						}},
					},
				})
			},
		},
		httperr.CodeCoercionFailed: {
			how: "an environment variable name that is not a legal env key",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromPatch(t, &paramPatchBody{
					Defaults: ptrDefaults(nil, map[string]json.RawMessage{"not a valid name": raw("x")}),
				})
			},
		},
		httperr.CodeUnknownField: {
			how: "a merge-patch body with a top-level key the schema does not define",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				c, rec := testGinContext(http.MethodPatch, "/zzrouter/v1/providers/vllm/parameters")
				body := []byte(`{"defalts":{"parameters":{"max-model-len":4096}}}`)
				if _, ok := decodePatchBody(c, body); ok {
					t.Fatal("a misspelled top-level field must be rejected, not accepted")
				}
				return codesFromParamErrors(t, rec)
			},
		},
		httperr.CodeRetired: {
			how: "a request to one of the retired parameter-write routes",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				c, rec := testGinContext(http.MethodPut, "/zzrouter/v1/providers/vllm/parameters")
				(&ParamsExecutor{}).RetiredHandler(c)
				if rec.Code != http.StatusGone {
					t.Fatalf("retired route should answer 410, got %d", rec.Code)
				}
				return codesFromProblem(t, rec)
			},
		},
		httperr.CodeRouteUnknown: {
			how: "a mutator addressing a model group that does not exist",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromMutatorError(t, modelgroup.ErrGroupNotFound)
			},
		},
		httperr.CodeReservedName: {
			how: "a PUT creating a model group named after a route literal",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				r := mountModelGroupsTestRouter(t, "version: \"1\"\nmodel_groups: {}\n")
				req := httptest.NewRequest(http.MethodPut, "/zzrouter/v1/model-groups/schema",
					strings.NewReader(`{"strategy":"priority","replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				return codesFromParamErrors(t, rec)
			},
		},
		httperr.CodeReplicaUnknown: {
			how: "a mutator addressing a replica not present in the group",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromMutatorError(t, modelgroup.ErrReplicaNotFound)
			},
		},
		httperr.CodeReplicaLastInGroup: {
			how: "deleting the only remaining replica in a group",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromMutatorError(t, modelgroup.ErrReplicaLastInGroup)
			},
		},
		httperr.CodeRouteNotClaimed: {
			how: "releasing ownership of a route that was never claimed",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromMutatorError(t, modelgroup.ErrRouteNotClaimed)
			},
		},
		httperr.CodeRequired: {
			how: "a request body missing a field the binder requires",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromBind(t, `{"kind":"chat"}`, &struct {
					Name string `json:"name" binding:"required"`
					Kind string `json:"kind"`
				}{})
			},
		},
		httperr.CodeInvalidValue: {
			how: "a request body carrying a value outside the field's allowed set",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				return codesFromBind(t, `{"strategy":"sideways"}`, &struct {
					Strategy string `json:"strategy" binding:"oneof=priority round_robin"`
				}{})
			},
		},
		httperr.CodePreflightFailed: {
			how: "an install whose preflight checks did not pass",
			provoke: func(t *testing.T) []httperr.ParamErrorCode {
				c, rec := testGinContext(http.MethodPost, "/zzrouter/v1/providers/vllm/install")
				respondPreflightFailed(c, &preflight.Report{
					Provider: "vllm",
					Results: []preflight.Result{
						{Check: "curl", Passed: false, Message: "curl not found", Hint: "install curl"},
					},
				})
				return codesFromParamErrors(t, rec)
			},
		},
	}
}

// codesFromBind runs the request binder over a body and reads the codes
// out of the rejection. The binder is where a body-validation failure
// becomes a per-key error, and it is the path that used to report an
// open-vocabulary validator tag instead of a code.
func codesFromBind(t *testing.T, body string, req any) []httperr.ParamErrorCode {
	t.Helper()
	RegisterValidatorTranslator()
	c, rec := testGinContext(http.MethodPost, "/zzrouter/v1/model-groups/g")
	c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/model-groups/g", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if BindJSON(c, req) {
		t.Fatalf("binder accepted %s, so it provoked nothing", body)
	}
	return codesFromParamErrors(t, rec)
}

// Every declared code must be provokable. This is the check that keeps
// the file honest as the enum grows.
func TestParamErrorCodes_AllAreReachable(t *testing.T) {
	cases := paramCodeCases()

	var missing []string
	for _, code := range httperr.AllParamErrorCodes() {
		if _, ok := cases[code]; !ok {
			missing = append(missing, string(code))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("no case provokes these declared codes: %v\n"+
			"Add one to paramCodeCases, or remove the constant if nothing can emit it.", missing)
	}

	declared := map[httperr.ParamErrorCode]bool{}
	for _, c := range httperr.AllParamErrorCodes() {
		declared[c] = true
	}
	for code := range cases {
		if !declared[code] {
			t.Errorf("case exists for %q, which the enum no longer declares", code)
		}
	}

	for code, tc := range cases {
		t.Run(string(code), func(t *testing.T) {
			got := tc.provoke(t)
			for _, g := range got {
				if g == code {
					return
				}
			}
			t.Errorf("provoking %q (%s) produced %v instead", code, tc.how, got)
		})
	}
}

// --- helpers -------------------------------------------------------

func testGinContext(method, path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, path, nil)
	return c, rec
}

func codesFromPatch(t *testing.T, patch *paramPatchBody) []httperr.ParamErrorCode {
	t.Helper()
	return codesFromPatchWithNodes(t, map[string]bool{}, patch)
}

func codesFromPatchWithNodes(t *testing.T, nodes map[string]bool, patch *paramPatchBody) []httperr.ParamErrorCode {
	t.Helper()
	errs := validatePatch("vllm", emptyCfgWithModel("llama-3-70b"), patch, vllmSchemaForTest(), patchScope{nodes: nodes})
	if len(errs) == 0 {
		t.Fatal("expected validatePatch to reject, got no errors")
	}
	return codesOf(errs)
}

func codesFromMutatorError(t *testing.T, err error) []httperr.ParamErrorCode {
	t.Helper()
	c, rec := testGinContext(http.MethodDelete, "/zzrouter/v1/model-groups/g/replicas/r")
	if !respondMutatorError(c, "g", "r", err) {
		t.Fatal("respondMutatorError reported no error for a non-nil error")
	}
	return codesFromParamErrors(t, rec)
}

// codesFromParamErrors reads the per-key entries out of the problem
// envelope. There is no second envelope to look in any more: a failure
// with per-key detail is a problem+json body whose `errors` array
// carries one entry per key.
func codesFromParamErrors(t *testing.T, rec *httptest.ResponseRecorder) []httperr.ParamErrorCode {
	t.Helper()
	var body utils.ProblemDetails
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem: %v (body=%s)", err, rec.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("problem carried no per-key errors (body=%s)", rec.Body.String())
	}
	return codesOf(body.Errors)
}

// codesFromProblem reads the TOP-LEVEL `code`. Failures that are not
// per-key carry it there and have no errors array, so a caller
// branching on `code` reads this one.
func codesFromProblem(t *testing.T, rec *httptest.ResponseRecorder) []httperr.ParamErrorCode {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v (body=%s)", err, rec.Body.String())
	}
	if problem.Code == "" {
		t.Fatalf("problem carried no top-level code (body=%s)", rec.Body.String())
	}
	return []httperr.ParamErrorCode{httperr.ParamErrorCode(problem.Code)}
}

func codesOf(errs []utils.ParamError) []httperr.ParamErrorCode {
	out := make([]httperr.ParamErrorCode, 0, len(errs))
	for _, e := range errs {
		out = append(out, httperr.ParamErrorCode(e.Code))
	}
	return out
}
