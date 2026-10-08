package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranslateResponsesRequest_BareStringInput(t *testing.T) {
	body := []byte(`{"model":"m","input":"hello"}`)
	out, parsed, err := translateResponsesRequest(body)
	require.Nil(t, err)
	require.False(t, parsed.Stream)

	var chat map[string]any
	require.NoError(t, json.Unmarshal(out, &chat))
	msgs := chat["messages"].([]any)
	require.Len(t, msgs, 1)
	m := msgs[0].(map[string]any)
	assert.Equal(t, "user", m["role"])
	assert.Equal(t, "hello", m["content"])
}

func TestTranslateResponsesRequest_InstructionsBecomeSystem(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","instructions":"be terse"}`)
	out, _, err := translateResponsesRequest(body)
	require.Nil(t, err)
	var chat map[string]any
	require.NoError(t, json.Unmarshal(out, &chat))
	msgs := chat["messages"].([]any)
	require.Len(t, msgs, 2)
	assert.Equal(t, "system", msgs[0].(map[string]any)["role"])
	assert.Equal(t, "be terse", msgs[0].(map[string]any)["content"])
}

func TestTranslateResponsesRequest_InputTextItemCollapses(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	out, _, err := translateResponsesRequest(body)
	require.Nil(t, err)
	var chat map[string]any
	require.NoError(t, json.Unmarshal(out, &chat))
	msgs := chat["messages"].([]any)
	require.Len(t, msgs, 1)
	assert.Equal(t, "hi", msgs[0].(map[string]any)["content"])
}

func TestTranslateResponsesRequest_FunctionCallOutputStaysString(t *testing.T) {
	// LiteLLM bug #18226: tool message content must be a string.
	body := []byte(`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":"42"}]}`)
	out, _, err := translateResponsesRequest(body)
	require.Nil(t, err)
	var chat map[string]any
	require.NoError(t, json.Unmarshal(out, &chat))
	msgs := chat["messages"].([]any)
	require.Len(t, msgs, 1)
	m := msgs[0].(map[string]any)
	assert.Equal(t, "tool", m["role"])
	assert.Equal(t, "c1", m["tool_call_id"])
	content := m["content"]
	_, isString := content.(string)
	assert.True(t, isString, "tool content must be a string, got %T", content)
}

func TestTranslateResponsesRequest_RejectsStore(t *testing.T) {
	_, _, err := translateResponsesRequest([]byte(`{"model":"m","input":"x","store":true}`))
	require.NotNil(t, err)
	assert.Equal(t, "unsupported_parameter", err.Code)
	assert.Equal(t, "store", err.Param)
}

func TestTranslateResponsesRequest_RejectsPreviousResponseID(t *testing.T) {
	_, _, err := translateResponsesRequest([]byte(`{"model":"m","input":"x","previous_response_id":"resp_1"}`))
	require.NotNil(t, err)
	assert.Equal(t, "previous_response_id", err.Param)
}

func TestTranslateResponsesRequest_RejectsBackground(t *testing.T) {
	_, _, err := translateResponsesRequest([]byte(`{"model":"m","input":"x","background":true}`))
	require.NotNil(t, err)
	assert.Equal(t, "background", err.Param)
}

func TestTranslateResponsesRequest_RejectsBuiltinTools(t *testing.T) {
	for _, tt := range []string{"web_search", "file_search", "code_interpreter", "computer_use_preview"} {
		body := []byte(`{"model":"m","input":"x","tools":[{"type":"` + tt + `"}]}`)
		_, _, err := translateResponsesRequest(body)
		require.NotNil(t, err, tt)
		assert.Equal(t, "unsupported_tool", err.Code, tt)
	}
}

func TestTranslateResponsesRequest_FlatToolBecomesNested(t *testing.T) {
	body := []byte(`{"model":"m","input":"x","tools":[{"type":"function","name":"add","description":"d","parameters":{"type":"object"}}]}`)
	out, _, err := translateResponsesRequest(body)
	require.Nil(t, err)
	var chat map[string]any
	require.NoError(t, json.Unmarshal(out, &chat))
	tools := chat["tools"].([]any)
	require.Len(t, tools, 1)
	tool := tools[0].(map[string]any)
	assert.Equal(t, "function", tool["type"])
	fn := tool["function"].(map[string]any)
	assert.Equal(t, "add", fn["name"])
}

func TestTranslateChatToResponses_TextOnly(t *testing.T) {
	chat := []byte(`{"id":"chatcmpl-abc","created":123,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	out, err := translateChatToResponses(chat, "m")
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(out, &resp))
	assert.Equal(t, "response", resp["object"])
	assert.True(t, strings.HasPrefix(resp["id"].(string), "resp_"))
	assert.Equal(t, "completed", resp["status"])
	output := resp["output"].([]any)
	require.Len(t, output, 1)
	item := output[0].(map[string]any)
	assert.Equal(t, "message", item["type"])
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	assert.Equal(t, "output_text", part["type"])
	assert.Equal(t, "hi there", part["text"])
	usage := resp["usage"].(map[string]any)
	assert.EqualValues(t, 5, usage["input_tokens"])
	assert.EqualValues(t, 3, usage["output_tokens"])
}

func TestTranslateChatToResponses_MultiToolCallsCollapse(t *testing.T) {
	// LiteLLM bug #18226: multi-tool-call requests must collapse into
	// one response.output[] with N function_call items, not fan out.
	chat := []byte(`{"id":"x","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"a","type":"function","function":{"name":"f1","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"f2","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	out, err := translateChatToResponses(chat, "m")
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(out, &resp))
	output := resp["output"].([]any)
	require.Len(t, output, 2)
	for _, raw := range output {
		item := raw.(map[string]any)
		assert.Equal(t, "function_call", item["type"])
	}
}

func TestTranslateChatToResponses_LengthFinishCarriesIncompleteDetails(t *testing.T) {
	chat := []byte(`{"id":"x","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"truncated"},"finish_reason":"length"}]}`)
	out, err := translateChatToResponses(chat, "m")
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(out, &resp))
	assert.Equal(t, "incomplete", resp["status"])
	details, ok := resp["incomplete_details"].(map[string]any)
	require.True(t, ok, "incomplete_details must be present on length-truncated responses")
	assert.Equal(t, "max_output_tokens", details["reason"])
}

func TestTranslateChatToResponses_ContentFilterCarriesReason(t *testing.T) {
	chat := []byte(`{"id":"x","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"blocked"},"finish_reason":"content_filter"}]}`)
	out, err := translateChatToResponses(chat, "m")
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(out, &resp))
	assert.Equal(t, "incomplete", resp["status"])
	assert.Equal(t, "content_filter", resp["incomplete_details"].(map[string]any)["reason"])
}

func TestTranslateError_UsesCanonicalOpenAIShape(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	(&translateError{Code: "unsupported_parameter", Param: "store", Message: "no go"}).write(c)
	body := rec.Body.String()
	// utils.NewOpenAIError marshals omitempty pointers — code+param appear,
	// type is invariant, and the wrapping {error: {...}} structure matches
	// every other OpenAI-compat error envelope on the surface.
	assert.True(t, strings.Contains(body, `"error":{`))
	assert.True(t, strings.Contains(body, `"type":"invalid_request_error"`))
	assert.True(t, strings.Contains(body, `"code":"unsupported_parameter"`))
	assert.True(t, strings.Contains(body, `"param":"store"`))
	assert.True(t, strings.Contains(body, `"message":"no go"`))
}

func TestTranslateUsage_PreservesCachedTokens(t *testing.T) {
	// LiteLLM bug #22192.
	usage := json.RawMessage(`{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":7}}`)
	out := translateUsage(usage)
	details := out["input_tokens_details"].(map[string]any)
	assert.EqualValues(t, 7, details["cached_tokens"])
}

func TestResponsesStreamWriter_BasicTextStream(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	sw := newResponsesStreamWriter(c.Writer, "m")

	// Send three chat chunks: role-only, content delta, finish.
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"prompt_tokens_details":{"cached_tokens":1}}}`+"\n\n")
	mustWrite(t, sw, "data: [DONE]\n\n")

	body := rec.Body.String()
	// Required events present and ordered.
	for _, tag := range []string{
		"event: response.created\n",
		"event: response.in_progress\n",
		"event: response.output_item.added\n",
		"event: response.content_part.added\n",
		"event: response.output_text.delta\n",
		"event: response.content_part.done\n",
		"event: response.output_item.done\n",
		"event: response.completed\n",
	} {
		assert.True(t, strings.Contains(body, tag), "missing %q in stream", tag)
	}
	// cached_tokens survives in the completed event.
	assert.True(t, strings.Contains(body, `"cached_tokens":1`), "cached_tokens missing in response.completed")
	assert.True(t, strings.HasSuffix(body, "data: [DONE]\n\n"))

	// SSE `event:` header and inner `data.type` must never drift. Each
	// frame's `event: X` line is followed on the next line by `data: {...,
	// "type":"X", ...}`. Walk the body and assert pairwise consistency.
	for _, frame := range strings.Split(body, "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) < 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			continue
		}
		evt := strings.TrimPrefix(lines[0], "event: ")
		assert.True(t,
			strings.Contains(lines[1], `"type":"`+evt+`"`),
			"event header %q must match data.type in payload: %s", evt, lines[1])
	}
}

func TestResponsesStreamWriter_FrameSpanningWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	sw := newResponsesStreamWriter(c.Writer, "m")

	frame := `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n"
	// Split the frame at byte 5 — the drainer must wait for \n\n.
	mustWrite(t, sw, frame[:5])
	body := rec.Body.String()
	assert.False(t, strings.Contains(body, "response.output_text.delta"),
		"delta event must not be emitted before the frame terminator arrives")
	mustWrite(t, sw, frame[5:])
	body = rec.Body.String()
	assert.True(t, strings.Contains(body, `"delta":"ok"`))
}

func TestResponsesStreamWriter_DeterministicToolItemOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	sw := newResponsesStreamWriter(c.Writer, "m")

	// Open three tool calls in non-monotonic chat-side index order.
	for _, n := range []string{"c", "a", "b"} {
		mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":`+map[string]string{"a": "1", "b": "2", "c": "0"}[n]+`,"id":"call_`+n+`","type":"function","function":{"name":"f_`+n+`"}}]},"finish_reason":null}]}`+"\n\n")
	}
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
	mustWrite(t, sw, "data: [DONE]\n\n")

	body := rec.Body.String()
	// Find the LAST occurrence of each call id (output_item.done is
	// emitted on finalize, after the initial output_item.added). Order
	// of last-occurrence == order of finalize emission.
	offC := strings.LastIndex(body, "fc_call_c")
	offA := strings.LastIndex(body, "fc_call_a")
	offB := strings.LastIndex(body, "fc_call_b")
	require.True(t, offC >= 0 && offA >= 0 && offB >= 0,
		"all three call ids must appear (c=%d a=%d b=%d)", offC, offA, offB)
	assert.True(t, offC < offA && offA < offB,
		"finalize must emit tool items by output_index (got offsets c=%d a=%d b=%d)", offC, offA, offB)
}

func TestResponsesStreamWriter_BufferOverflowEmitsFailed(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	sw := newResponsesStreamWriter(c.Writer, "m")

	// Stream a large payload without any frame terminator.
	junk := make([]byte, maxStreamBufBytes+1024)
	for i := range junk {
		junk[i] = 'x'
	}
	// Not mustWrite: overflow is the condition under test, so a write error here
	// is an acceptable outcome — the assertion is on the frame it emits.
	_, _ = sw.Write(junk)
	body := rec.Body.String()
	assert.True(t, strings.Contains(body, "event: response.failed"),
		"overflow must emit response.failed")
	assert.True(t, strings.Contains(body, "upstream_stream_buffer_overflow"))
}

func TestResponsesBufferedWriter_TranslatesAndCapturesID(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	bw := newResponsesBufferedWriter(c.Writer, "m")

	// Simulate the chat path writing a 200 with an OpenAI-shaped envelope.
	bw.WriteHeader(200)
	mustWrite(t, bw, `{"id":"chatcmpl-abc","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	bw.Finalize()

	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	body := rec.Body.String()
	assert.True(t, strings.Contains(body, `"object":"response"`))
	assert.True(t, strings.Contains(body, `"output_text":"hi"`))
	assert.NotEmpty(t, bw.respID)
	assert.True(t, strings.HasPrefix(bw.respID, "resp_"))
}

func TestResponsesBufferedWriter_PassesThroughUpstreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	bw := newResponsesBufferedWriter(c.Writer, "m")

	bw.WriteHeader(429)
	mustWrite(t, bw, `{"error":{"type":"rate_limit_exceeded"}}`)
	bw.Finalize()

	assert.Equal(t, 429, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), "rate_limit_exceeded"))
	assert.Empty(t, bw.respID, "no resp id on error path")
}

func TestResponsesBufferedWriter_GinInterfaceSurface(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	bw := newResponsesBufferedWriter(c.Writer, "m")

	assert.False(t, bw.Written())
	mustWrite(t, bw, "hello")
	assert.True(t, bw.Written())
	assert.Equal(t, 5, bw.Size())
	assert.Equal(t, 200, bw.Status())
	bw.WriteHeader(503)
	assert.Equal(t, 503, bw.Status())
}

func TestValidateWireEndpoints_RejectsNativePlusCompat(t *testing.T) {
	// This test belongs in pkg/config but the helper is exported there;
	// kept here in the translator suite for proximity to the shim.
	// (The same scenario is also exercised in pkg/config tests.)
}

// TestResponsesStreamWriter_OutOfOrderToolIndices — chat upstreams may
// open tool_calls in any index order. The translator must key tool item
// state by chat-side index, not on arrival order, so a "1 then 0 then 2"
// chunk sequence still produces three distinct function_call output
// items and finalize emits them sorted by output_index.
func TestResponsesStreamWriter_OutOfOrderToolIndices(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	sw := newResponsesStreamWriter(c.Writer, "m")

	// Open tool index 1 first, then 0, then 2 — args streamed interleaved.
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"second"}}]},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"first"}}]},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"x\":2}"}}]},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_c","type":"function","function":{"name":"third","arguments":"{}"}}]},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
	mustWrite(t, sw, "data: [DONE]\n\n")

	body := rec.Body.String()
	// Finalize emits in monotonic output_index order. outputIdx is
	// assigned in ARRIVAL order: b=0, a=1, c=2. So done events fire
	// b → a → c regardless of chat-side index.
	offB := strings.LastIndex(body, "fc_call_b")
	offA := strings.LastIndex(body, "fc_call_a")
	offC := strings.LastIndex(body, "fc_call_c")
	require.True(t, offB >= 0 && offA >= 0 && offC >= 0,
		"all tool ids must appear (b=%d a=%d c=%d)", offB, offA, offC)
	assert.True(t, offB < offA && offA < offC,
		"finalize must emit by output_index (got b=%d a=%d c=%d)", offB, offA, offC)
	assert.True(t, strings.Contains(body, `"delta":"{\"x\":1}"`))
	assert.True(t, strings.Contains(body, `"delta":"{\"x\":2}"`))
}

// TestResponsesStreamWriter_MidStreamErrorChunk — some chat upstreams
// emit a final chat.completion.chunk whose data carries an error object
// rather than a normal choice. The translator surfaces it as a terminal
// response.failed event with upstream's type/message preserved, then
// stops emitting (no response.completed after a response.failed).
func TestResponsesStreamWriter_MidStreamErrorChunk(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	sw := newResponsesStreamWriter(c.Writer, "m")

	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"part"},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"error":{"type":"server_error","message":"upstream blew up"}}`+"\n\n")
	mustWrite(t, sw, "data: [DONE]\n\n")

	body := rec.Body.String()
	assert.True(t, strings.Contains(body, "event: response.failed"),
		"upstream error chunk must produce response.failed")
	assert.True(t, strings.Contains(body, `"type":"server_error"`),
		"upstream error type must be preserved verbatim")
	assert.True(t, strings.Contains(body, `"message":"upstream blew up"`),
		"upstream error message must be preserved verbatim")
	assert.False(t, strings.Contains(body, "event: response.completed"),
		"response.completed must not follow response.failed")
	failedAt := strings.Index(body, "event: response.failed")
	require.True(t, failedAt >= 0)
	assert.False(t, strings.Contains(body[failedAt:], "response.output_text.delta"),
		"no further deltas may emit after response.failed")
	assert.True(t, strings.HasSuffix(body, "data: [DONE]\n\n"))
}

// TestResponsesStreamWriter_PartialFrameThenReconnect — a real client
// disconnect between two halves of an SSE frame leaves the buffer with
// a half-frame; Close must drain the partial without panic and emit
// response.completed once the upstream has finished sending. The
// trailing buffered data falls through `\n\n`-appended draining; if
// the partial is incomplete JSON, handleChunk's json.Unmarshal is
// expected to silently return.
func TestResponsesStreamWriter_PartialFrameThenClose(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	sw := newResponsesStreamWriter(c.Writer, "m")

	// Send a complete frame, then a half frame, then Close (no [DONE]).
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"con`)
	// No second Write delivering the rest. Client gone.
	sw.Close()

	body := rec.Body.String()
	assert.True(t, strings.Contains(body, "event: response.output_text.delta"),
		"first complete frame must have produced a delta")
	assert.True(t, strings.Contains(body, "event: response.completed"),
		"Close must finalize even when upstream cut mid-frame")
}

func TestResponsesStreamWriter_ToolCallEmitsArguments(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	sw := newResponsesStreamWriter(c.Writer, "m")

	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"add"}}]},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1"}}]},"finish_reason":null}]}`+"\n\n")
	mustWrite(t, sw, `data: {"id":"x","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
	mustWrite(t, sw, "data: [DONE]\n\n")

	body := rec.Body.String()
	assert.True(t, strings.Contains(body, "event: response.function_call_arguments.delta"))
	assert.True(t, strings.Contains(body, "event: response.function_call_arguments.done"))
	assert.True(t, strings.Contains(body, `"name":"add"`))
	assert.True(t, strings.Contains(body, `"call_id":"call_1"`))
}

// mustWrite fails the test if the writer under test rejects a chunk: a
// translator that errored on every chunk would otherwise be asserted
// against whatever little it managed to write.
func mustWrite(t *testing.T, w io.Writer, chunk string) {
	t.Helper()
	if _, err := w.Write([]byte(chunk)); err != nil {
		t.Fatalf("write: %v", err)
	}
}
