package protocol

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeChatExtensions(t *testing.T) {
	chat := map[string]any{"model": "routed", "messages": []any{}}
	require.NoError(t, MergeChatExtensions(chat, json.RawMessage(`{"seed":9007199254740993,"min_p":0.01,"native":{"enabled":false}}`)))
	out, err := json.Marshal(chat)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"seed":9007199254740993`)
	assert.Contains(t, string(out), `"native":{"enabled":false}`)
	for _, field := range []string{"model", "messages", "tools", "Tools", "tool_choice", "stream", "max_tokens", "response_format", "reasoning_effort"} {
		t.Run(field, func(t *testing.T) {
			target := map[string]any{"model": "routed"}
			err := MergeChatExtensions(target, json.RawMessage(`{"seed":1,"`+field+`":null}`))
			var ext *ChatExtensionError
			require.ErrorAs(t, err, &ext)
			assert.Equal(t, "extra_params."+field, ext.Param)
			assert.Equal(t, map[string]any{"model": "routed"}, target, "refusal must not partially merge fields")
		})
	}
	for _, raw := range []string{`[]`, `true`, `"text"`} {
		assert.Error(t, MergeChatExtensions(chat, json.RawMessage(raw)))
	}
}

func TestMergeChatExtensions_RefusesUnrepresentableOutputs(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"n", `2`}, {"logprobs", `true`}, {"top_logprobs", `2`},
		{"audio", `{"voice":"alloy"}`}, {"modalities", `["text","audio"]`},
	} {
		t.Run(tc.key, func(t *testing.T) {
			for _, nested := range []bool{false, true} {
				chat := map[string]any{"model": "m"}
				var extras json.RawMessage
				param := tc.key
				if nested {
					extras = json.RawMessage(`{"` + tc.key + `":` + tc.value + `}`)
					param = "extra_params." + tc.key
				} else {
					chat[tc.key] = json.RawMessage(tc.value)
				}
				var ext *ChatExtensionError
				require.ErrorAs(t, MergeChatExtensions(chat, extras), &ext)
				assert.Equal(t, param, ext.Param)
			}
		})
	}
	assert.NoError(t, MergeChatExtensions(map[string]any{}, json.RawMessage(`{"n":1,"logprobs":false,"top_logprobs":0,"audio":null,"modalities":["text"]}`)))
}
