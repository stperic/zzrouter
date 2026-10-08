package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tool-using turn as Claude Code sends it: a system prompt in blocks,
// a user turn of several text blocks, the assistant's thinking, text and
// two calls, then the two results (one failed) with a follow-up.
const toolConversation = `{
  "model": "qwen", "max_tokens": 1024, "stream": true,
  "system": [{"type": "text", "text": "You are a coder."}, {"type": "text", "text": "Be brief.", "cache_control": {"type": "ephemeral"}}],
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "<reminder>"}, {"type": "text", "text": "List and read."}]},
    {"role": "assistant", "content": [
      {"type": "thinking", "thinking": "plan", "signature": "s"},
      {"type": "text", "text": "Looking."},
      {"type": "tool_use", "id": "toolu_1", "name": "ls", "input": {"path": "."}},
      {"type": "tool_use", "id": "toolu_2", "name": "read", "input": {"file": "x"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.go\nb.go"},
      {"type": "tool_result", "tool_use_id": "toolu_2", "is_error": true, "content": [{"type": "text", "text": "no such file"}]},
      {"type": "text", "text": "Go on."}
    ]}
  ],
  "tools": [
    {"name": "ls", "description": "List files", "input_schema": {"type": "object"}},
    {"type": "web_search_20250305", "name": "web_search"}
  ],
  "tool_choice": {"type": "auto", "disable_parallel_tool_use": true},
  "stop_sequences": ["END"], "temperature": 0.2, "top_k": 20,
  "metadata": {"user_id": "u1"},
  "thinking": {"type": "enabled", "budget_tokens": 1000}
}`

func TestToChatRequest_ToolConversation(t *testing.T) {
	got, err := ToChatRequest([]byte(toolConversation))
	require.NoError(t, err)
	assert.Equal(t, "qwen", got.Model)
	assert.True(t, got.Stream)
	assert.JSONEq(t, `{
	  "model": "qwen", "max_tokens": 1024, "stream": true, "stream_options": {"include_usage": true},
	  "stop": ["END"], "temperature": 0.2, "top_k": 20, "user": "u1",
	  "messages": [
	    {"role": "system", "content": "You are a coder.\n\nBe brief."},
	    {"role": "user", "content": "<reminder>\n\nList and read."},
	    {"role": "assistant", "content": "Looking.", "tool_calls": [
	      {"id": "toolu_1", "type": "function", "function": {"name": "ls", "arguments": "{\"path\": \".\"}"}},
	      {"id": "toolu_2", "type": "function", "function": {"name": "read", "arguments": "{\"file\": \"x\"}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "toolu_1", "content": "a.go\nb.go"},
	    {"role": "tool", "tool_call_id": "toolu_2", "content": "Error: no such file"},
	    {"role": "user", "content": "Go on."}
	  ],
	  "tools": [{"type": "function", "function": {"name": "ls", "description": "List files", "parameters": {"type": "object"}}}],
	  "tool_choice": "auto", "parallel_tool_calls": false
	}`, string(got.Body))
}

func TestToChatRequest_ShapesAndChoices(t *testing.T) {
	translate := func(body string) map[string]any {
		t.Helper()
		got, err := ToChatRequest([]byte(body))
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(got.Body, &out))
		return out
	}

	plain := translate(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, []any{map[string]any{"role": "user", "content": "hi"}}, plain["messages"])
	assert.NotContains(t, plain, "stream_options", "no stream, no usage frame to ask for")

	image := translate(`{"model":"m","messages":[{"role":"user","content":[
	  {"type":"text","text":"what?"},
	  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]}]}`)
	assert.Equal(t, []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what?"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
	}}}, image["messages"], "an image keeps the turn as parts")

	tool := `{"name":"t","input_schema":{"type":"object"}}`
	for choice, want := range map[string]any{
		`{"type":"any"}`:             "required",
		`{"type":"none"}`:            "none",
		`{"type":"tool","name":"t"}`: map[string]any{"type": "function", "function": map[string]any{"name": "t"}},
	} {
		out := translate(`{"model":"m","messages":[],"tools":[` + tool + `],"tool_choice":` + choice + `}`)
		assert.Equal(t, want, out["tool_choice"], choice)
	}

	onlyServerTools := translate(`{"model":"m","messages":[],"tools":[{"type":"web_search_20250305","name":"w"}],"tool_choice":{"type":"any"}}`)
	assert.NotContains(t, onlyServerTools, "tools")
	assert.NotContains(t, onlyServerTools, "tool_choice", "no tools left to choose from")
}

func TestToChatRequest_Refusals(t *testing.T) {
	for name, tc := range map[string]struct{ body, param string }{
		"not json":        {`{`, "body"},
		"bad role":        {`{"model":"m","messages":[{"role":"tool","content":"x"}]}`, "messages.0.role"},
		"context image":   {`{"model":"m","messages":[{"role":"ctx","content":[{"type":"image"}]}]}`, "messages.0.content.0"},
		"document block":  {`{"model":"m","messages":[{"role":"user","content":[{"type":"document"}]}]}`, "messages.0.content.0"},
		"image no source": {`{"model":"m","messages":[{"role":"user","content":[{"type":"image"}]}]}`, "messages.0.content.0.source"},
		"system image":    {`{"model":"m","system":[{"type":"image"}],"messages":[]}`, "system"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ToChatRequest([]byte(tc.body))
			var terr *TranslateError
			require.ErrorAs(t, err, &terr)
			assert.Equal(t, tc.param, terr.Param)
		})
	}
}

func TestFromChatReply(t *testing.T) {
	reply := `{"id":"chatcmpl-1","choices":[{"finish_reason":"tool_calls","message":{
	  "content":"Reading.","reasoning_content":"need the file",
	  "tool_calls":[{"id":"call_1","function":{"name":"read","arguments":"{\"file\":\"x\"}"}},
	                {"function":{"name":"ls","arguments":"not json"}}]}}],
	  "usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":60}}}`
	got, err := FromChatReply([]byte(reply), "qwen+agent")
	require.NoError(t, err)
	assert.JSONEq(t, `{
	  "id": "msg_chatcmpl-1", "type": "message", "role": "assistant", "model": "qwen+agent",
	  "content": [
	    {"type": "thinking", "thinking": "need the file", "signature": ""},
	    {"type": "text", "text": "Reading."},
	    {"type": "tool_use", "id": "call_1", "name": "read", "input": {"file": "x"}},
	    {"type": "tool_use", "id": "toolu_1", "name": "ls", "input": {}}
	  ],
	  "stop_reason": "tool_use", "stop_sequence": null,
	  "usage": {"input_tokens": 40, "output_tokens": 7, "cache_read_input_tokens": 60}
	}`, string(got))

	for finish, want := range map[string]string{"stop": "end_turn", "length": "max_tokens", "content_filter": "refusal", "": "end_turn"} {
		out, err := FromChatReply([]byte(`{"choices":[{"finish_reason":"`+finish+`","message":{"content":"x"}}]}`), "m")
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(out, &m))
		assert.Equal(t, want, m["stop_reason"], finish)
	}

	_, err = FromChatReply([]byte(`{"choices":[]}`), "m")
	assert.Error(t, err)
}

func TestCountTokens(t *testing.T) {
	req, err := ToCountTokensRequest([]byte(`{"model":"m","stream":true,"max_tokens":9,"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	assert.False(t, req.Stream)
	assert.JSONEq(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, string(req.Body))

	got, err := FromChatTokenCount([]byte(`{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":42,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":40}}}`), "m")
	require.NoError(t, err)
	assert.JSONEq(t, `{"input_tokens":42}`, string(got))

	_, err = FromChatTokenCount([]byte(`{"choices":[]}`), "m")
	assert.Error(t, err, "no usage, no count")
}

// Claude Code 2.1.154 on sends system, ctx and msg turns inside messages;
// each is a system message where it stands.
func TestToChatRequest_ContextRoles(t *testing.T) {
	got, err := ToChatRequest([]byte(`{"model":"m","messages":[
	  {"role":"user","content":"hi"},
	  {"role":"system","content":"agent prompt"},
	  {"role":"ctx","content":[{"type":"text","text":"cwd: /x"}]},
	  {"role":"msg","content":"note"},
	  {"role":"assistant","content":"ok"}]}`))
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(got.Body, &out))
	assert.Equal(t, []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "system", "content": "agent prompt"},
		map[string]any{"role": "system", "content": "cwd: /x"},
		map[string]any{"role": "system", "content": "note"},
		map[string]any{"role": "assistant", "content": "ok"},
	}, out["messages"])
}

func TestTranslate_Edges(t *testing.T) {
	// A named tool_choice whose tool was left out (a server tool) names
	// nothing the engine has.
	got, err := ToChatRequest([]byte(`{"model":"m","messages":[],
	  "tools":[{"name":"t","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"w"}],
	  "tool_choice":{"type":"tool","name":"w"}}`))
	require.NoError(t, err)
	assert.NotContains(t, string(got.Body), "tool_choice")

	// More cached than prompt tokens never makes input negative; a reply
	// with calls stops for tool_use even when the engine says stop; no id
	// still gives an id.
	out, err := FromChatReply([]byte(`{"choices":[{"finish_reason":"stop","message":{"tool_calls":[{"id":"c","function":{"name":"ls","arguments":"{}"}}]}}],
	  "usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":9}}}`), "m")
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, float64(0), m["usage"].(map[string]any)["input_tokens"])
	assert.Equal(t, "tool_use", m["stop_reason"])
	assert.Equal(t, anonymousMessageID, m["id"])
}

func TestToChatRequest_ToolResultImages(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[
	{"type":"tool_result","tool_use_id":"a","is_error":true,"content":[{"type":"text","text":"screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
	{"type":"text","text":"describe both"},
	{"type":"tool_result","tool_use_id":"b","content":[{"type":"image","source":{"type":"url","url":"https://example.test/image"}}]}]}]}`)
	for _, translate := range []func([]byte) (ChatRequest, error){ToChatRequest, ToCountTokensRequest} {
		got, err := translate(body)
		require.NoError(t, err)
		var out map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(got.Body, &out))
		assert.JSONEq(t, `[
		{"role":"tool","tool_call_id":"a","content":"Error: screenshot"},
		{"role":"tool","tool_call_id":"b","content":""},
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},{"type":"text","text":"describe both"},{"type":"image_url","image_url":{"url":"https://example.test/image"}}]}]`, string(out["messages"]))
	}
	_, err := ToChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image"}]}]}]}`))
	require.ErrorContains(t, err, "messages.0.content.0.content.0.source")
}
