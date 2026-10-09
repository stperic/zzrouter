package httperr_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/httperr"
	anthropic "github.com/stperic/zzrouter/pkg/protocol/anthropic"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestNormalizeUpstreamResponse(t *testing.T) {
	for _, tc := range []struct {
		name, raw    string
		status, want int
		typ          string
	}{
		{"object", `{"error":{"message":"image input is not supported","type":"api_error"}}`, 500, 400, "invalid_request_error"},
		{"flat", `{"error":"this model does not support images"}`, 500, 400, "invalid_request_error"},
		{"not found", `{"error":"model not found"}`, 404, 404, "not_found_error"},
		{"native", `{"type":"error","error":{"type":"api_error","message":"images are not supported"}}`, 500, 400, "invalid_request_error"},
		{"plain", `image input is not supported`, 500, 400, "invalid_request_error"},
		{"multimodal decode", `{"error":"Failed to decode multimodal data"}`, 500, 500, "api_error"},
		{"unsupported template", `{"error":"chat template does not support tool calls"}`, 500, 500, "api_error"},
		{"template refusal", `{"error":{"message":"Error: Jinja Exception: System message must be at the beginning.","type":"server_error"}}`, 500, 400, "invalid_request_error"},
		{"template refusal not 500", `{"error":"Jinja Exception: x"}`, 503, 400, "invalid_request_error"},
		{"oom", `{"error":"image allocation failed: out of memory"}`, 500, 500, "api_error"},
		{"prompt echo", `{"request":{"text":"images are not supported"},"error":"out of memory"}`, 500, 500, "api_error"},
		{"numeric code with prompt echo", `{"request":{"text":"images are not supported"},"error":"out of memory","code":500}`, 500, 500, "api_error"},
		{"bad request", `{"error":"bad input"}`, 400, 400, "invalid_request_error"},
		{"auth", `{"error":"bad key"}`, 401, 401, "authentication_error"},
		{"permission", `{"error":"forbidden"}`, 403, 403, "permission_error"},
		{"rate", `{"error":"too many"}`, 429, 429, "rate_limit_error"},
		{"not 500", `{"error":"images are not supported"}`, 503, 400, "invalid_request_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, compressed := range []bool{false, true} {
				raw := []byte(tc.raw)
				h := http.Header{"Retry-After": []string{"60"}}
				if compressed {
					var b bytes.Buffer
					w := gzip.NewWriter(&b)
					_, err := w.Write(raw)
					require.NoError(t, err)
					require.NoError(t, w.Close())
					raw = b.Bytes()
					h.Set("Content-Encoding", "gzip")
				}
				resp := &http.Response{StatusCode: tc.status, Header: h, Body: io.NopCloser(bytes.NewReader(raw))}
				got := httperr.NormalizeUpstreamResponse(resp, openaiproto.New(), testClassifier(t, "llamacpp"))
				assert.Equal(t, tc.want, resp.StatusCode)
				assert.Equal(t, tc.typ, got.Type)
				out, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				assert.Empty(t, resp.Header.Get("Content-Encoding"))
				assert.EqualValues(t, len(out), resp.ContentLength)
				if tc.want != tc.status {
					assert.Contains(t, string(out), `"type":"invalid_request_error"`)
					code := "unsupported_input"
					if strings.Contains(tc.raw, "Jinja Exception") {
						code = httperr.CodeChatTemplateRejected
					}
					assert.Contains(t, string(out), `"code":"`+code+`"`)
					assert.Empty(t, resp.Header.Get("Retry-After"))
				} else {
					assert.Equal(t, "60", resp.Header.Get("Retry-After"))
				}
			}
		})
	}
}

func TestNormalizeUpstreamResponseAnthropicAndUnreadable(t *testing.T) {
	resp := &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"images are not supported","type":"api_error"}}`))}
	_ = httperr.NormalizeUpstreamResponse(resp, anthropic.New(nil), testClassifier(t, "llamacpp"))
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode)
	assert.JSONEq(t, `{"type":"error","error":{"type":"invalid_request_error","message":"images are not supported Use a model that supports this input, or remove it from the request.","code":"unsupported_input"}}`, string(out))
	for _, raw := range []string{strings.Repeat("x", 1024*1024), "bad gzip"} {
		resp = &http.Response{StatusCode: 500, Header: http.Header{"Content-Encoding": []string{"gzip"}}, Body: io.NopCloser(strings.NewReader(raw))}
		got := httperr.NormalizeUpstreamResponse(resp, openaiproto.New(), testClassifier(t, "llamacpp"))
		assert.Equal(t, 500, resp.StatusCode)
		assert.Equal(t, "api_error", got.Type)
	}
}

// These messages come from pinned upstream sources, not captured engine traffic.
func TestNormalizeSourceVerifiedCapabilityRefusals(t *testing.T) {
	files, err := filepath.Glob("testdata/capability_refusals/*.json")
	require.NoError(t, err)
	require.Len(t, files, 3)
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			require.NoError(t, err)
			var fixture struct {
				Source string          `json:"source"`
				Error  json.RawMessage `json:"error"`
			}
			require.NoError(t, json.Unmarshal(raw, &fixture))
			require.Contains(t, fixture.Source, "github.com/")
			body, err := json.Marshal(map[string]json.RawMessage{"error": fixture.Error})
			require.NoError(t, err)
			resp := &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
			provider := "llamacpp"
			if strings.Contains(file, "vllm") {
				provider = "vllm"
			}
			failure := httperr.NormalizeUpstreamResponse(resp, openaiproto.New(), testClassifier(t, provider))
			assert.Equal(t, 400, failure.Status)
			assert.Equal(t, "unsupported_input", failure.Code)
		})
	}
}

// The engine's own words reach the caller with the way to fix it, as a 400
// a client will not retry.
func TestNormalizeChatTemplateRefusal(t *testing.T) {
	raw, err := os.ReadFile("testdata/template_refusals/llamacpp.json")
	require.NoError(t, err)
	var fixture struct {
		Source string          `json:"source"`
		Reason string          `json:"reason"`
		Error  json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Contains(t, fixture.Source, "github.com/")
	body, err := json.Marshal(map[string]json.RawMessage{"error": fixture.Error})
	require.NoError(t, err)
	resp := &http.Response{StatusCode: 500, Header: http.Header{"Retry-After": {"1"}}, Body: io.NopCloser(bytes.NewReader(body))}

	failure := httperr.NormalizeUpstreamResponse(resp, openaiproto.New(), testClassifier(t, "llamacpp"))

	assert.Equal(t, http.StatusBadRequest, failure.Status)
	assert.Equal(t, httperr.CodeChatTemplateRejected, failure.Code)
	assert.Contains(t, failure.Message, fixture.Reason)
	assert.Contains(t, failure.Message, httperr.ChatTemplateRemedy)
	assert.NotContains(t, failure.Message, "CallExpression", "the template source excerpt is not the reason")
	assert.Empty(t, resp.Header.Get("Retry-After"))
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"code":"chat_template_rejected"`)
}

// vLLM launched without its tool-calling switches refuses every tools
// request; the caller learns what to set instead of a bare engine error.
func TestNormalizeToolCallingNotEnabled(t *testing.T) {
	raw, err := os.ReadFile("testdata/tool_refusals/vllm.json")
	require.NoError(t, err)
	var fixture struct {
		Error json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	body, err := json.Marshal(map[string]json.RawMessage{"error": fixture.Error})
	require.NoError(t, err)
	resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}

	failure := httperr.NormalizeUpstreamResponse(resp, openaiproto.New(), testClassifier(t, "vllm"))

	assert.Equal(t, http.StatusBadRequest, failure.Status)
	assert.Equal(t, httperr.CodeToolCallingNotEnabled, failure.Code)
	assert.Contains(t, failure.Message, "--tool-call-parser")
	assert.Contains(t, failure.Message, httperr.ToolCallingRemedy)
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"code":"tool_calling_not_enabled"`)

	// The coordinator relays the worker's classified reply: once is enough.
	again := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(out))}
	relayed := httperr.NormalizeUpstreamResponse(again, openaiproto.New(), nil)
	assert.Equal(t, httperr.CodeToolCallingNotEnabled, relayed.Code)
	assert.Equal(t, failure.Message, relayed.Message)
}

func TestChatTemplateRefusalReasonIsBounded(t *testing.T) {
	for name, tc := range map[string]struct{ reason, want string }{
		"empty": {"", "(no reason given)."},
		"long":  {strings.Repeat("x", 5000), strings.Repeat("x", 300) + "..."},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"error": map[string]string{"message": "Error: Jinja Exception: " + tc.reason}})
			require.NoError(t, err)
			resp := &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
			failure := httperr.NormalizeUpstreamResponse(resp, openaiproto.New(), testClassifier(t, "llamacpp"))
			assert.Equal(t, httperr.CodeChatTemplateRejected, failure.Code)
			assert.Contains(t, failure.Message, "request: "+tc.want+" ")
			assert.Less(t, len(failure.Message), 600)
		})
	}
}

func testClassifier(t *testing.T, provider string) *httperr.Classifier {
	t.Helper()
	kind := "on-demand"
	if provider == "ollama" {
		kind = "external"
	}
	raw, err := templates.AppsFS.ReadFile("files/providers/" + kind + "/" + provider + "/config.yaml")
	require.NoError(t, err)
	var cfg config.ServiceConfig
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.NoError(t, cfg.Runtime.FailurePatterns.Compile())
	return &cfg.Runtime.FailurePatterns
}
