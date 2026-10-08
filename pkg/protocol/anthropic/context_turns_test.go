package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func foldedMessages(t *testing.T, body string) []map[string]any {
	t.Helper()
	got, err := FoldContextTurns([]byte(body))
	require.NoError(t, err)
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(got, &out))
	return out.Messages
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// The shape Claude Code sends (captured 2026-10-08): its environment block
// is a system turn after the user's. It joins that user turn, so no system
// message follows the first and every chat template can render it.
func TestFoldContextTurns_ClaudeCodeShape(t *testing.T) {
	got := foldedMessages(t, `{"model":"m","system":[{"type":"text","text":"sys"}],"messages":[
	  {"role":"user","content":[{"type":"text","text":"<system-reminder>r</system-reminder>"},{"type":"text","text":"Say hi"}]},
	  {"role":"system","content":[{"type":"text","text":"# Environment"}]}]}`)
	assert.Equal(t, []map[string]any{{"role": "user", "content": []any{
		text("<system-reminder>r</system-reminder>"), text("Say hi"), text("# Environment")}}}, got)
}

func TestFoldContextTurns_Placement(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []map[string]any
	}{
		"joins the following user turn": {
			`{"messages":[{"role":"ctx","content":"c"},{"role":"user","content":"u"}]}`,
			[]map[string]any{{"role": "user", "content": []any{text("c"), text("u")}}},
		},
		"joins the previous user turn when an assistant follows": {
			`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":"agent prompt"},
			  {"role":"ctx","content":[{"type":"text","text":"cwd: /x"}]},{"role":"msg","content":"note"},
			  {"role":"assistant","content":"ok"}]}`,
			[]map[string]any{
				{"role": "user", "content": []any{text("hi"), text("agent prompt"), text("cwd: /x"), text("note")}},
				{"role": "assistant", "content": "ok"},
			},
		},
		"stays after an assistant turn instead of rewriting the user turn before it": {
			`{"messages":[{"role":"user","content":"u"},{"role":"assistant","content":"a"},{"role":"system","content":"c"}]}`,
			[]map[string]any{
				{"role": "user", "content": "u"},
				{"role": "assistant", "content": "a"},
				{"role": "user", "content": []any{text("c")}},
			},
		},
		"follows the tool results a user turn opens with": {
			`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
			  {"role":"ctx","content":"c"},
			  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"next"}]}]}`,
			[]map[string]any{
				{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "f", "input": map[string]any{}}}},
				{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}, text("c"), text("next")}},
			},
		},
		"becomes a user turn when there is none": {
			`{"messages":[{"role":"ctx","content":"c"},{"role":"assistant","content":"a"}]}`,
			[]map[string]any{{"role": "user", "content": []any{text("c")}}, {"role": "assistant", "content": "a"}},
		},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, tc.want, foldedMessages(t, tc.body)) })
	}
}

// Turns the fold does not touch keep their bytes, so a conversation's
// prefix stays the same from one request to the next; block fields such
// as cache_control travel with the text.
func TestFoldContextTurns_KeepsBytes(t *testing.T) {
	first := `{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}`
	second := `{"role":"assistant","content":"b"}`
	body := `{"model":"m","messages":[` + first + `,` + second + `,{"role":"user","content":"c"},{"role":"ctx","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`
	got, err := FoldContextTurns([]byte(body))
	require.NoError(t, err)
	var out struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(got, &out))
	assert.Equal(t, "m", out.Model)
	assert.Equal(t, first, string(out.Messages[0]))
	assert.Equal(t, second, string(out.Messages[1]))
	assert.JSONEq(t, `{"role":"user","content":[{"type":"text","text":"c"},{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}`, string(out.Messages[2]))

	again, err := FoldContextTurns(got)
	require.NoError(t, err)
	assert.Equal(t, string(got), string(again), "folding is idempotent")

	plain := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	unchanged, err := FoldContextTurns(plain)
	require.NoError(t, err)
	assert.Equal(t, string(plain), string(unchanged))
}

func TestFoldContextTurns_RefusesNonText(t *testing.T) {
	_, err := FoldContextTurns([]byte(`{"messages":[{"role":"ctx","content":[{"type":"image","source":{}}]}]}`))
	var terr *TranslateError
	require.ErrorAs(t, err, &terr)
	assert.Equal(t, "messages.0.content.0", terr.Param)
}
