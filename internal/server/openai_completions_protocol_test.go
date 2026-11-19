package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompletionsPromptShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt string
		valid  bool
	}{
		{"omitted", "", true}, {"null", "null", true}, {"empty_text", `""`, true},
		{"text", `"hello"`, true}, {"text_batch", `["hello",""]`, true},
		{"tokens", `[1, 9007199254740993]`, true}, {"token_batch", `[[1,2],[3]]`, true},
		{"empty_batch", `[]`, false}, {"empty_tokens", `[[]]`, false},
		{"object", `{}`, false}, {"number", `42`, false}, {"mixed", `["hi",1]`, false},
		{"negative", `[-1]`, false}, {"fraction", `[1.5]`, false}, {"null_token", `[null]`, false},
		{"null_text", `["hi",null]`, false}, {"null_batch", `[[1],null]`, false}, {"nested_text", `[["hi"]]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"native-model","max_tokens":4,"native":{"large":9007199254740993}`
			if tc.prompt != "" {
				body += `,"prompt":` + tc.prompt
			}
			body += "}"
			called := false
			h := OpenAIInferenceHandlers{dispatchWithRecorder: func(c *gin.Context, model string, got []byte, recorder *llm.InferenceRecorder, _ trace.Span, endpoint string) {
				called = true
				assert.Equal(t, body, string(got))
				assert.Equal(t, "native-model", model)
				assert.Equal(t, "chat", endpoint)
				require.NotNil(t, recorder)
				assert.Equal(t, got, c.Request.Context().Value(CtxKeyOriginalBody))
				c.Status(http.StatusNoContent)
				c.Writer.WriteHeaderNow()
			}}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(body))
			h.handleCompletions(c)
			assert.Equal(t, tc.valid, called)
			if tc.valid {
				assert.Equal(t, http.StatusNoContent, w.Code)
			} else {
				assert.Equal(t, http.StatusBadRequest, w.Code)
				assert.Contains(t, w.Body.String(), `"param":"prompt"`)
			}
		})
	}
}

// Synthetic engines exercise the production group/fallback and model rewrite paths.
func TestCompletionsRichPromptReachesFallbackWire(t *testing.T) {
	backends := map[string]*serviceBackend{
		"busy":  {wire: compatWire, status: http.StatusServiceUnavailable, reply: `{"error":{"message":"busy"}}`},
		"ready": {wire: compatWire, status: http.StatusOK, reply: `{"id":"completion","choices":[{"text":"ok","finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`},
	}
	s := newServiceGroupNode(t, []string{"busy", "ready"}, backends)
	w := postAsAdmin(t, s, "/v1/completions", `{"model":"claude","prompt":[[1,9007199254740993],[2]],"seed":9007199254740993,"max_tokens":4}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	for name, backend := range backends {
		hits := backend.requests()
		require.NotEmpty(t, hits)
		for _, hit := range hits {
			assert.Equal(t, "/v1/completions", hit.Path)
			assert.Contains(t, string(hit.Body), `"prompt":[[1,9007199254740993],[2]]`)
			assert.Contains(t, string(hit.Body), `"model":"`+name+`-model"`)
			assert.Contains(t, string(hit.Body), `"seed":9007199254740993`)
		}
	}
}

func TestCompletionRichPromptKeepsLogParameters(t *testing.T) {
	for _, prompt := range []string{`[1,2]`, `[[1],[2]]`, `["a","b"]`, `"a"`} {
		entry := &inferencelog.LogEntry{}
		(&InferenceLogBridge{}).extractRequestDetails(entry, []byte(`{"prompt":`+prompt+`,"temperature":0.2,"max_tokens":4,"top_p":0.9}`))
		require.NotNil(t, entry.MaxTokens)
		assert.Equal(t, 4, *entry.MaxTokens)
		require.NotNil(t, entry.Temperature)
		assert.Equal(t, 0.2, *entry.Temperature)
		if prompt == `"a"` {
			require.Len(t, entry.Messages, 1)
		} else {
			assert.Empty(t, entry.Messages)
		}
	}
}
