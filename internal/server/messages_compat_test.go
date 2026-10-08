package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// compatWire is what a Chat-only engine behind messages_compat declares.
var compatWire = []string{"chat_completions", messagesCompatEndpoint}

const chatReplyJSON = `{"id":"c9","choices":[{"finish_reason":"stop","message":{"content":"Hello."}}],
  "usage":{"prompt_tokens":12,"completion_tokens":3}}`

// A Messages request for a model on a Chat-only engine reaches the engine
// as Chat Completions on the chat path, and the client gets a Messages
// reply under the name it asked for.
func TestMessagesCompat_TranslatesBothWays(t *testing.T) {
	backends := map[string]*serviceBackend{"mlxish": {wire: compatWire, status: http.StatusOK, reply: chatReplyJSON}}
	s := newServiceGroupNode(t, []string{"mlxish"}, backends)
	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(captureHook(func(d llm.InferenceLogData) { captured = d }))
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })

	w := postAsAdmin(t, s, "/v1/messages",
		`{"model":"claude","max_tokens":8,"system":"Be kind.","messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"id":"msg_c9","type":"message","role":"assistant","model":"claude",
	  "content":[{"type":"text","text":"Hello."}],"stop_reason":"end_turn","stop_sequence":null,
	  "usage":{"input_tokens":12,"output_tokens":3}}`, w.Body.String())

	assert.Equal(t, int64(12), captured.TokensIn, "metered from the engine's chat usage")
	assert.Equal(t, int64(3), captured.TokensOut)

	hits := backends["mlxish"].requests()
	require.Len(t, hits, 1)
	assert.Equal(t, chatCompletionsPath, hits[0].Path, "the engine is asked on the chat path")
	var sent map[string]any
	require.NoError(t, json.Unmarshal(hits[0].Body, &sent))
	assert.Equal(t, "mlxish-model", sent["model"], "the replica's model, not the alias")
	assert.Equal(t, []any{
		map[string]any{"role": "system", "content": "Be kind."},
		map[string]any{"role": "user", "content": "hi"},
	}, sent["messages"])
}

func TestMessagesCompat_Streams(t *testing.T) {
	chatStream := `data: {"id":"c5","choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"id":"c5","choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"c5","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	backends := map[string]*serviceBackend{"mlxish": {wire: compatWire, status: http.StatusOK,
		reply: chatStream, contentType: "text/event-stream"}}
	s := newServiceGroupNode(t, []string{"mlxish"}, backends)

	w := postAsAdmin(t, s, "/v1/messages",
		`{"model":"claude","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
	body := w.Body.String()
	for _, event := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		assert.Contains(t, body, "event: "+event+"\n", body)
	}
	assert.Contains(t, body, `"text":"Hel"`)
	assert.Contains(t, body, `"usage":{"input_tokens":7,"output_tokens":2}`)
	assert.NotContains(t, body, "[DONE]", "a Messages stream ends with message_stop")

	var sent map[string]any
	require.NoError(t, json.Unmarshal(backends["mlxish"].requests()[0].Body, &sent))
	assert.Equal(t, map[string]any{"include_usage": true}, sent["stream_options"], "the reply's usage comes from it")
}

// A group whose replicas differ is narrowed to one way: native when it
// has a native replica, so the request is not translated per replica.
func TestMessagesCompat_MixedGroupPrefersNative(t *testing.T) {
	backends := map[string]*serviceBackend{
		"mlxish": {wire: compatWire, status: http.StatusOK, reply: chatReplyJSON},
		"native": {wire: []string{"chat_completions", messagesEndpoint}, status: http.StatusOK},
	}
	s := newServiceGroupNode(t, []string{"mlxish", "native"}, backends)

	w := postAsAdmin(t, s, "/v1/messages", `{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	assert.Empty(t, backends["mlxish"].requests(), "the compat replica is narrowed away")
	require.Len(t, backends["native"].requests(), 1)
	assert.Equal(t, "/v1/messages", backends["native"].requests()[0].Path)
}

// count_tokens on a Chat-only engine is the prompt usage of a one-token
// completion.
func TestMessagesCompat_CountTokens(t *testing.T) {
	backends := map[string]*serviceBackend{"mlxish": {wire: compatWire, status: http.StatusOK, reply: chatReplyJSON}}
	s := newServiceGroupNode(t, []string{"mlxish"}, backends)

	w := postAsAdmin(t, s, "/v1/messages/count_tokens", `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	assert.JSONEq(t, `{"input_tokens":12}`, w.Body.String())
	hit := backends["mlxish"].requests()[0]
	var sent map[string]any
	require.NoError(t, json.Unmarshal(hit.Body, &sent))
	assert.Equal(t, float64(1), sent["max_tokens"])
	assert.NotContains(t, sent, "stream")
	assert.Equal(t, "1", hit.Headers.Get(headerNotInference),
		"a worker this reaches runs it as chat, so it must be told it is not inference")
}

// Only the worker's cluster engine, which a coordinator alone reaches,
// believes the not-inference header; a client-facing engine drops a copy
// a client sent, so it cannot reach a worker either.
func TestNotInferenceHeader_TrustedOnlyFromCoordinator(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	var forwarded string
	s.engine.POST("/zz-echo", func(c *gin.Context) { forwarded = c.Request.Header.Get(headerNotInference) })
	req := httptest.NewRequest(http.MethodPost, "/zz-echo", nil)
	req.Header.Set(headerNotInference, "1")
	s.engine.ServeHTTP(httptest.NewRecorder(), req)
	assert.Empty(t, forwarded, "a client's copy is dropped at the door")

	worker := gin.New()
	worker.Use(clusterHeadersFromCoordinator())
	var marked bool
	worker.POST("/v1/chat/completions", func(c *gin.Context) {
		marked = requestRecorder(c.Request.Context(), "m", "p") == nil
	})
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(headerNotInference, "1")
	worker.ServeHTTP(httptest.NewRecorder(), req)
	assert.True(t, marked, "the worker's cluster engine records no inference for it")
}

// A 200 the engine sent that is not Chat Completions is a 502 in the
// Anthropic dialect, whole: the engine's length must not cut it short.
func TestMessagesCompat_UntranslatableReplyIs502(t *testing.T) {
	backends := map[string]*serviceBackend{"mlxish": {wire: compatWire, status: http.StatusOK, reply: `{"not":"chat"}`}}
	s := newServiceGroupNode(t, []string{"mlxish"}, backends)
	node := httptest.NewServer(s.engine)
	t.Cleanup(node.Close)

	req, err := http.NewRequest(http.MethodPost, node.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", TestAdminKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "the body arrives whole")
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	errType, _ := decodeAnthropicError(t, body)
	assert.Equal(t, "api_error", errType)
}

// What goes wrong is said in the Anthropic dialect, whether the engine
// fails or the request cannot be translated.
func TestMessagesCompat_ErrorsSpeakAnthropic(t *testing.T) {
	backends := map[string]*serviceBackend{"mlxish": {wire: compatWire, status: http.StatusInternalServerError,
		reply: `{"error":{"message":"kv cache full","type":"server_error"}}`}}
	s := newServiceGroupNode(t, []string{"mlxish"}, backends)

	w := postAsAdmin(t, s, "/v1/messages", `{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	require.GreaterOrEqual(t, w.Code, http.StatusInternalServerError, "body: %s", w.Body)
	errType, _ := decodeAnthropicError(t, w.Body.Bytes())
	assert.NotEmpty(t, errType, "an Anthropic error envelope")

	w = postAsAdmin(t, s, "/v1/messages",
		`{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body)
	errType, msg := decodeAnthropicError(t, w.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errType)
	assert.True(t, strings.Contains(msg, "document"), msg)
}

// The fallback copy path synthesizes DONE on EOF; it must not hide truncation.
func TestMessagesCompat_TruncatedFallbackStreamReturnsError(t *testing.T) {
	backends := map[string]*serviceBackend{"mlxish": {wire: compatWire, status: http.StatusOK,
		reply: `data: {"id":"cut","choices":[{"delta":{"content":"partial"}}]}` + "\n\n", contentType: "text/event-stream"}}
	s := newServiceGroupNode(t, []string{"mlxish"}, backends)
	w := postAsAdmin(t, s, "/v1/messages", `{"model":"claude","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	assert.Contains(t, w.Body.String(), "event: error\n")
	assert.NotContains(t, w.Body.String(), "event: message_stop\n")
	assert.NotContains(t, w.Body.String(), `"stop_reason":"end_turn"`)
}
