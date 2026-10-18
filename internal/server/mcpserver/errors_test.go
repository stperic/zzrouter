package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/httperr"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	commonmcp "github.com/stperic/common/mcp"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func requireToolError(t *testing.T, err error) *toolError {
	t.Helper()
	var failure *toolError
	require.ErrorAs(t, err, &failure)
	return failure
}

func toolClient(t *testing.T, d dispatcher) (*mcp.ClientSession, context.Context) {
	t.Helper()
	return connectToolClient(t, d, newServer(d.cfg))
}

func connectToolClient(t *testing.T, d dispatcher, server *mcp.Server) (*mcp.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	handler := commonmcp.HTTPHandler(server, commonmcp.HTTPOptions{MaxRequestBytes: MaxRequestBytes})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, values := range d.requestInfo.Header {
			r.Header[name] = append([]string(nil), values...)
		}
		r.RemoteAddr = d.requestInfo.RemoteAddr
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), outerKey, d.outer)))
	}))
	t.Cleanup(httpServer.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "error-contract-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cs.Close()) })
	return cs, ctx
}

func callTool(t *testing.T, d dispatcher, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	client, ctx := toolClient(t, d)
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	return result
}

func resultProblem(t *testing.T, result *mcp.CallToolResult) utils.ProblemDetails {
	t.Helper()
	require.True(t, result.IsError)
	require.NotNil(t, result.StructuredContent)
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(raw, &problem))
	require.NotZero(t, problem.Status)
	require.NotEmpty(t, problem.Code)
	require.NotEmpty(t, problem.Type)
	require.NotEmpty(t, problem.Title)
	return problem
}

func problemFixture(t *testing.T) (dispatcher, utils.ProblemDetails) {
	t.Helper()
	problem := *utils.NewProblemDetails(400, "Bad Request", "Fix provider parameters", "/zzrouter/v1/runs/preview")
	problem.Code, problem.Node, problem.RequestID = "wrong_type", "worker", "rest-request-123"
	problem.Errors = []utils.ParamError{
		{Key: "ctx", Code: "wrong_type", Got: "many", Want: "integer", Message: "expected integer", Hint: "read zz_params_get"},
		{Key: "gpu", Code: "out_of_range", Got: float64(99), Want: "number", Min: float64(0), Max: float64(8), Message: "too large", Hint: "read zz_params_get"},
	}
	d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
		w.WriteHeader(problem.Status)
		require.NoError(t, json.NewEncoder(w).Encode(problem))
	})
	return d, problem
}

func previewError(t *testing.T, d dispatcher) *mcp.CallToolResult {
	t.Helper()
	return callTool(t, d, "zz_run_preview", map[string]any{"model": "m", "provider": "p"})
}

func TestToolErrorCarriesProblemStructuredContent(t *testing.T) {
	d, want := problemFixture(t)
	result := previewError(t, d)
	require.Equal(t, want, resultProblem(t, result))
	require.Nil(t, result.GetError(), "the client receives the problem, not the server's error field")
}

func TestToolErrorTextBlockPresent(t *testing.T) {
	d, _ := problemFixture(t)
	result := previewError(t, d)
	require.True(t, result.IsError)
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	require.Equal(t, "HTTP 400 wrong_type: Fix provider parameters\nctx: expected integer\ngpu: too large\nread zz_params_get", text.Text, "a hint the issues share is said once")
}

func TestToolErrorKeepsRequestID(t *testing.T) {
	d, want := problemFixture(t)
	problem := resultProblem(t, previewError(t, d))
	require.Equal(t, want.RequestID, problem.RequestID)
	require.Equal(t, want.Node, problem.Node)
	require.Equal(t, want.Instance, problem.Instance)
}

func TestToolErrorKeepsPerKeyErrors(t *testing.T) {
	d, want := problemFixture(t)
	require.Equal(t, want.Errors, resultProblem(t, previewError(t, d)).Errors)
}

func TestToolErrorNonProblemFailuresAreProblems(t *testing.T) {
	for _, tc := range []struct {
		name, tool, code string
		status           int
		args             map[string]any
		configure        func(*dispatcher)
	}{
		{"non-JSON 502", "zz_update_status", "bad_gateway", 502, map[string]any{}, func(d *dispatcher) {
			d.cfg.Engine = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(502)
				writeFixture(t, w, []byte("upstream unavailable"))
			})
		}},
		{"cap overflow", "zz_update_status", string(codeResponseTooLarge), 502, map[string]any{}, func(d *dispatcher) {
			d.cfg.Engine = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := w.Write([]byte(strings.Repeat("x", MaxResponseBytes+1)))
				require.Error(t, err)
			})
		}},
		{"oversized 500 keeps its status", "zz_update_status", utils.NewProblemDetails(500, http.StatusText(500), "", "").Code, 500, map[string]any{}, func(d *dispatcher) {
			d.cfg.Engine = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(strings.Repeat("x", MaxResponseBytes+1)))
			})
		}},
		{"streaming refusal", "zz_update_status", string(codeStreamingRefused), 400, map[string]any{}, func(d *dispatcher) { d.cfg.Streaming = func(string, string) string { return "sse" } }},
		{"pre-dispatch validation", "zz_job_cancel", string(codeInvalidInput), 400, map[string]any{}, func(*dispatcher) {}},
		{"response decode", "zz_update_status", string(codeResponseDecode), 502, map[string]any{}, func(d *dispatcher) {
			d.cfg.Engine = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeFixture(t, w, []byte("not JSON")) })
		}},
		{"interrupted request", "zz_update_status", string(codeInterrupted), httperr.StatusClientClosedRequest, map[string]any{}, func(d *dispatcher) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			d.outer.ctx = ctx
		}},
		{"search failure", "zz_api_search", string(codeInternal), 500, map[string]any{"query": "runs"}, func(d *dispatcher) {
			d.cfg.SearchAPI = func(string, int) ([]map[string]any, error) { return nil, errors.New("invalid embedded spec") }
		}},
		{"catalog refusal", "zz_api_get", string(codeInvalidInput), 400, map[string]any{"path": "/zzrouter/v1/mcp"}, func(d *dispatcher) {
			d.cfg.EligibleAPIRoute = func(string, string) (string, error) { return "", errors.New("catalog refused route") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected REST dispatch") })
			tc.configure(&d)
			problem := resultProblem(t, callTool(t, d, tc.tool, tc.args))
			require.Equal(t, tc.status, problem.Status)
			require.Equal(t, tc.code, problem.Code)
		})
	}
}

func TestToolErrorMiddlewareIgnoresOtherErrors(t *testing.T) {
	failure := newToolError(400, codeInvalidInput, "bad input", nil)
	for _, tc := range []struct {
		name, method string
		result       mcp.Result
		err          error
	}{
		{"ordinary error", "tools/call", &mcp.CallToolResult{}, errors.New("ordinary failure")},
		{"success", "tools/call", &mcp.CallToolResult{}, nil},
		{"other method", "tools/list", &mcp.CallToolResult{}, failure},
		{"other result", "tools/call", &mcp.ListToolsResult{}, nil},
		{"nil result", "tools/call", nil, errors.New("protocol failure")},
		{"typed nil", "tools/call", (*mcp.CallToolResult)(nil), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if call, ok := tc.result.(*mcp.CallToolResult); ok && call != nil {
				call.Content = []mcp.Content{&mcp.TextContent{Text: "keep content"}}
				call.StructuredContent = map[string]any{"keep": "structure"}
				if tc.err != nil {
					call.SetError(tc.err)
				}
			}
			before, err := json.Marshal(tc.result)
			require.NoError(t, err)
			nextErr := errors.New("keep handler error")
			wrapped := commonmcp.ErrorMiddleware(func(context.Context, string, mcp.Request) (mcp.Result, error) { return tc.result, nextErr })
			got, err := wrapped(context.Background(), tc.method, nil)
			require.Same(t, nextErr, err)
			after, err := json.Marshal(got)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestToolErrorMiddlewareUnwrapsAndOnlySetsStructuredContent(t *testing.T) {
	failure := newToolError(400, codeInvalidInput, "bad input", nil)
	err := fmt.Errorf("caller: %w", failure)
	call := &mcp.CallToolResult{}
	call.SetError(err)
	content := call.Content
	wrapped := commonmcp.ErrorMiddleware(func(context.Context, string, mcp.Request) (mcp.Result, error) { return call, nil })
	got, outerErr := wrapped(context.Background(), "tools/call", nil)
	require.NoError(t, outerErr)
	require.Same(t, call, got)
	require.Equal(t, failure.problem, call.StructuredContent)
	require.Equal(t, content, call.Content)
	require.True(t, call.IsError)
	require.Same(t, err, call.GetError())
}

func TestToolErrorEveryRegisteredTool(t *testing.T) {
	args := map[string]map[string]any{
		"zz_overview": {}, "zz_nodes": {}, "zz_providers": {}, "zz_models": {}, "zz_runs": {}, "zz_logs": {},
		"zz_update_status": {}, "zz_update_check": {}, "zz_update_apply": {},
		"zz_model_search": {"query": "m"}, "zz_model_files": {"model": "org/m"}, "zz_model_download": {"model": "m"}, "zz_model_delete": {"model": "m"},
		"zz_provider_install": {"provider": "p"}, "zz_run_preview": {"model": "m", "provider": "p"},
		"zz_run_start": {"model": "m", "provider": "p"}, "zz_run_restart": {"run_id": "r"}, "zz_run_stop": {"run_id": "r"},
		"zz_chat": {"model": "m", "message": "hi"}, "zz_job": {"job_id": "j"}, "zz_job_cancel": {"job_id": "j"},
		"zz_params_get": {"provider": "p"}, "zz_params_set": {"provider": "p", "patch": map[string]any{}},
		"zz_api_search": {"query": "runs"}, "zz_api_get": {"path": "/zzrouter/v1/models"}, "zz_api": {"method": "POST", "path": "/zzrouter/v1/runs"},
	}
	d, _ := problemFixture(t)
	d.cfg.SearchAPI = func(string, int) ([]map[string]any, error) { return nil, errors.New("spec unavailable") }
	d.cfg.EligibleAPIRoute = func(string, string) (string, error) { return "", nil }
	client, ctx := toolClient(t, d)
	listed, err := client.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, listed.NextCursor)
	require.Len(t, listed.Tools, len(args))
	for _, tool := range listed.Tools {
		t.Run(tool.Name, func(t *testing.T) {
			input, ok := args[tool.Name]
			require.True(t, ok, "add a schema-valid error variant for this tool")
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: input})
			require.NoError(t, err)
			problem := resultProblem(t, result)
			if tool.Name == "zz_api_search" {
				require.Equal(t, 500, problem.Status)
				require.Equal(t, string(codeInternal), problem.Code)
			} else {
				require.Equal(t, 400, problem.Status)
				require.Equal(t, "wrong_type", problem.Code)
			}
		})
	}
}

func TestToolErrorFailedJobIsSuccess(t *testing.T) {
	d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
		writeFixture(t, w, []byte(`{"data":{"job_id":"j","phase":"failed","err":"launch failed"}}`))
	})
	result := callTool(t, d, "zz_job", map[string]any{"job_id": "j", "wait_seconds": 45})
	require.False(t, result.IsError)
	require.Equal(t, "failed", result.StructuredContent.(map[string]any)["state"])
	require.Equal(t, "launch failed", result.StructuredContent.(map[string]any)["error"])
	require.NotContains(t, result.StructuredContent.(map[string]any), "code")
}

func TestToolErrorHandlerValidation(t *testing.T) {
	for _, tc := range []struct{ tool, args string }{
		{"zz_api_search", `{"query":"runs","limit":-1}`},
		{"zz_api", `{"method":"GET","path":"/zzrouter/v1/models"}`},
		{"zz_api_get", `{"path":"/v1/models"}`},
		{"zz_job", `{"job_id":".."}`},
		{"zz_job", `{"job_id":"j","wait_seconds":46}`},
		{"zz_models", `{"offset":-1}`},
		{"zz_models", `{"limit":1001}`},
		{"zz_job_cancel", `{}`},
		{"zz_job_cancel", `{"job_id":"j","deployment_id":"d"}`},
		{"zz_job_cancel", `{"deployment_id":"d","node":"worker"}`},
		{"zz_chat", `{"model":"m","message":"hi","max_tokens":-1}`},
		{"zz_logs", `{"run_id":"r","lines":-1}`},
	} {
		t.Run(tc.tool+tc.args, func(t *testing.T) {
			var args map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.args), &args))
			d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid input dispatched") })
			problem := resultProblem(t, callTool(t, d, tc.tool, args))
			require.Equal(t, 400, problem.Status)
			require.Equal(t, string(codeInvalidInput), problem.Code)
		})
	}
}

func TestToolErrorResponseShapeDecode(t *testing.T) {
	for _, tc := range []struct {
		tool, body string
		args       map[string]any
	}{
		{"zz_model_search", `{"data":[]}`, map[string]any{"query": "m"}},
		{"zz_models", `{"data":{}}`, map[string]any{}},
		{"zz_models", `{"data":[1]}`, map[string]any{}},
		{"zz_chat", `{"choices":[]}`, map[string]any{"model": "m", "message": "hi"}},
	} {
		t.Run(tc.tool+tc.body, func(t *testing.T) {
			d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) { writeFixture(t, w, []byte(tc.body)) })
			problem := resultProblem(t, callTool(t, d, tc.tool, tc.args))
			require.Equal(t, 502, problem.Status)
			require.Equal(t, string(codeResponseDecode), problem.Code)
		})
	}
}

func TestToolErrorInternalFailures(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"zz_api_search", map[string]any{"query": "runs"}},
		{"zz_api_get", map[string]any{"path": "/zzrouter/v1/models"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("internal failure dispatched") })
			problem := resultProblem(t, callTool(t, d, tc.tool, tc.args))
			require.Equal(t, 500, problem.Status)
			require.Equal(t, string(codeInternal), problem.Code)
		})
	}
	d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("unencodable input dispatched") })
	var out any
	failure := requireToolError(t, d.call(context.Background(), request{jsonBody: make(chan int)}, &out))
	require.Equal(t, 500, failure.problem.Status)
	require.Equal(t, string(codeInternal), failure.problem.Code)
	_, err := newDispatcher(context.Background(), d.cfg, nil)
	failure = requireToolError(t, err)
	require.Equal(t, 500, failure.problem.Status)
	require.Equal(t, string(codeInternal), failure.problem.Code)
	failure = requireToolError(t, d.call(context.Background(), request{method: "bad method", path: modelsRoute.path()}, &out))
	require.Equal(t, 400, failure.problem.Status)
	require.Equal(t, string(codeInvalidInput), failure.problem.Code)
}

func TestToolErrorSDKValidationIsUntouched(t *testing.T) {
	d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("SDK-invalid input dispatched") })
	result := callTool(t, d, "zz_job", map[string]any{"job_id": 42})
	require.True(t, result.IsError)
	require.Nil(t, result.StructuredContent)
	require.NotEmpty(t, result.Content)
}

func TestToolErrorCallbacksKeepWrappedProblems(t *testing.T) {
	for _, name := range []string{"zz_api_search", "zz_api_get"} {
		t.Run(name, func(t *testing.T) {
			failure := newToolError(503, codeInternal, "catalog unavailable", nil)
			failure.problem.RequestID = "original-request"
			wrapped := fmt.Errorf("callback: %w", failure)
			d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("callback error dispatched") })
			d.cfg.SearchAPI = func(string, int) ([]map[string]any, error) { return nil, wrapped }
			d.cfg.EligibleAPIRoute = func(string, string) (string, error) { return "", wrapped }
			args := map[string]any{"query": "runs"}
			if name == "zz_api_get" {
				args = map[string]any{"path": "/zzrouter/v1/models"}
			}
			result := callTool(t, d, name, args)
			require.Equal(t, failure.problem, resultProblem(t, result))
			require.Equal(t, "callback: catalog unavailable", result.Content[0].(*mcp.TextContent).Text)
		})
	}
}

func TestToolErrorNilPatchValidation(t *testing.T) {
	d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("nil patch dispatched") })
	_, err := d.patch(context.Background(), patchInput{})
	failure := requireToolError(t, err)
	require.Equal(t, 400, failure.problem.Status)
	require.Equal(t, string(codeInvalidInput), failure.problem.Code)
}

// A problem without a code is still the server's problem: its request_id survives and the status fills the code.
func TestToolErrorProblemWithoutCodeKeepsFields(t *testing.T) {
	d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"title":"Conflict","detail":"busy","request_id":"rest-req-9"}`))
	})
	problem := resultProblem(t, callTool(t, d, "zz_update_status", map[string]any{}))
	require.Equal(t, http.StatusConflict, problem.Status)
	require.Equal(t, "rest-req-9", problem.RequestID)
	require.Equal(t, utils.NewProblemDetails(http.StatusConflict, http.StatusText(http.StatusConflict), "", "").Code, problem.Code)
}

func TestToolErrorKeepsPinnedMemoryProblem(t *testing.T) {
	problem := *utils.NewProblemDetails(http.StatusConflict, "Conflict", "reclaim skipped: pinned", "/zzrouter/v1/runs")
	problem.Code = "out_of_range"
	problem.Errors = []utils.ParamError{{Key: "budget", Code: "out_of_range", Got: map[string]any{
		"holders": []any{map[string]any{"run": "embedding-run", "model": "embedding", "reclaim_skipped_reason": "pinned"}},
	}}}
	d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(problem.Status)
		require.NoError(t, json.NewEncoder(w).Encode(problem))
	})
	result := callTool(t, d, "zz_run_start", map[string]any{"provider": "vllm", "model": "large"})
	require.Equal(t, problem, resultProblem(t, result))
}
