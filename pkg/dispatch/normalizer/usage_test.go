package normalizer

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatUsageNormalization(t *testing.T) {
	input := `{"id":9007199254740993,"model":"m","choices":[{"message":{"content":"","reasoning":"thinking","tool_calls":[{"function":{"arguments":"{\"cost\":5}"}}],"usage":{"cost":8}}}],"usage":{"prompt_tokens":19,"completion_tokens":3,"total_tokens":22,"cost":0,"cost_details":{"upstream_inference_cost":0},"is_byok":false,"unknown":{"cost":7},"prompt_tokens_details":{"cached_tokens":2,"cache_write_tokens":1,"audio_tokens":3},"completion_tokens_details":{"reasoning_tokens":1,"cache_write_tokens":4}},"cost":6}`
	for _, provider := range []string{"unknown", "mlx"} {
		t.Run(provider, func(t *testing.T) {
			r := NewRegistry()
			r.CleanChatUsage = func() bool { return true }
			RegisterMLX(r)
			for _, stream := range []bool{false, true} {
				body := r.Body(provider)([]byte(input))
				if stream {
					body = r.Stream(provider)([]byte("data: " + input + "\n\n"))
					body = []byte(strings.TrimSpace(strings.TrimPrefix(string(body), "data: ")))
				}
				assert.Contains(t, string(body), `9007199254740993`)
				var obj map[string]any
				require.NoError(t, json.Unmarshal(body, &obj))
				usage := obj["usage"].(map[string]any)
				for _, key := range []string{"cost", "cost_details", "is_byok"} {
					assert.NotContains(t, usage, key)
				}
				assert.Equal(t, float64(19), usage["prompt_tokens"])
				assert.Equal(t, float64(3), usage["completion_tokens"])
				assert.Equal(t, float64(22), usage["total_tokens"])
				assert.Equal(t, map[string]any{"cached_tokens": float64(2), "audio_tokens": float64(3)}, usage["prompt_tokens_details"])
				assert.Equal(t, map[string]any{"reasoning_tokens": float64(1), "cache_write_tokens": float64(4)}, usage["completion_tokens_details"])
				assert.Equal(t, map[string]any{"cost": float64(7)}, usage["unknown"])
				assert.Equal(t, float64(6), obj["cost"])
				msg := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
				assert.Equal(t, map[string]any{"cost": float64(8)}, msg["usage"])
				assert.Contains(t, string(body), `"arguments":"{\"cost\":5}"`)
				if provider == "mlx" {
					assert.Equal(t, "thinking", msg["content"])
					assert.Equal(t, "thinking", msg["reasoning"])
				}
				assert.Equal(t, body, r.Body(provider)(body))
			}
		})
	}
}

func TestChatUsageNormalizationNoOp(t *testing.T) {
	r := NewRegistry()
	r.CleanChatUsage = func() bool { return true }
	for _, input := range []string{
		` {"choices":[],"usage":null} `,
		` {"choices":[],"usage":{"prompt_tokens":1}} `,
		` {"usage":{"cost":2},"object":"response"} `,
		` {"data":[],"usage":{"cost":2}} `,
		` {"object":"text_completion","choices":[],"usage":{"cost":2}} `,
		` {"message":{"usage":{"cost":2}}} `,
		` {"choices":null,"usage":{"cost":2}} `,
		` {"choices":{},"usage":{"cost":2}} `,
		` {"choices":[],"usage":{"cost":2}`,
		` {"choices":[],"usage":{"cost":2}} trailing`,
		`null`, `[]`,
	} {
		assert.Equal(t, input, string(r.Body("p")([]byte(input))))
	}
}

func TestNormalizationSkipsDecode(t *testing.T) {
	for _, provider := range []string{"openrouter", "ollama", "mlx"} {
		for _, clean := range []bool{false, true} {
			t.Run(provider+"/cleanup="+strconv.FormatBool(clean), func(t *testing.T) {
				r := NewRegistry()
				r.CleanChatUsage = func() bool { return clean }
				edits := 0
				if provider == "mlx" {
					r.Register(provider, func(map[string]any) bool { edits++; return false })
				}
				for _, input := range []string{
					` {"choices":[{"delta":{"content":"Hello"}}], "usage":null} `,
					` {"model":"llama3","message":{"content":"Hello"},"done":false} `,
				} {
					body := []byte(input)
					if normalize := r.Body(provider); normalize != nil {
						assert.Zero(t, testing.AllocsPerRun(10, func() { normalize(body) }), "untouched bodies must not allocate a decoder")
						assert.Equal(t, body, normalize(body))
					}
					if normalize := r.Stream(provider); normalize != nil {
						frame := []byte("data: " + input + "\r\n\r\n")
						assert.LessOrEqual(t, testing.AllocsPerRun(10, func() { normalize(frame) }), float64(1), "only the frame splitter may allocate")
						assert.Equal(t, frame, normalize(frame))
					}
				}
				assert.Zero(t, edits, "frames without editor keys must never reach the object editor")
			})
		}
	}
	// Reasoning only triggers decoding when this provider has an editor.
	r := NewRegistry()
	r.CleanChatUsage = func() bool { return true }
	RegisterMLX(r)
	input := []byte(` {"choices":[{"delta":{"reasoning":"thinking"}}]} `)
	normalize := r.Body("openrouter")
	assert.Zero(t, testing.AllocsPerRun(10, func() { normalize(input) }))
	assert.Equal(t, input, normalize(input))
}

func TestNormalizationEditorKeys(t *testing.T) {
	r := NewRegistry()
	r.CleanChatUsage = func() bool { return true }
	for _, field := range []string{
		`"cost":0`,
		`"cost_details":{}`,
		`"is_byok":true`,
		`"prompt_tokens_details":{"cache_write_tokens":1}`,
	} {
		input := []byte(`{"choices":[],"usage":{` + field + `}}`)
		assert.NotEqual(t, input, r.Body("openrouter")(input), field)
		frame := []byte("data: " + string(input) + "\r\n\r\n")
		assert.NotEqual(t, frame, r.Stream("openrouter")(frame), field)
	}
}

func TestNormalizationFraming(t *testing.T) {
	r := NewRegistry()
	r.CleanChatUsage = func() bool { return true }
	RegisterMLX(r)
	for _, tc := range []struct{ input, want string }{
		{"\t data: {\"choices\":[],\"usage\":{\"cost\":1}} \t\r\n\r\n", "\t data: {\"choices\":[],\"usage\":{}} \t\r\n\r\n"},
		{"\t {\"choices\":[],\"usage\":{\"cost\":1}} \r\n", "\t {\"choices\":[],\"usage\":{}} \r\n"},
		{"data: {\"choices\":[{\"delta\":{\"reasoning\":\"r\"}}],\"usage\":{\"cost\":0}}\r\n", "data: {\"choices\":[{\"delta\":{\"content\":\"r\",\"reasoning\":\"r\"}}],\"usage\":{}}\r\n"},
		{" : keep-alive\r\n\tdata: [DONE]\r\n\r\n", " : keep-alive\r\n\tdata: [DONE]\r\n\r\n"},
		{" data: {\"choices\":[],\"usage\":{\"cost\":1}  \r\n", " data: {\"choices\":[],\"usage\":{\"cost\":1}  \r\n"},
		{"data: {\"choices\":[], \"usage\":null} \r\n", "data: {\"choices\":[], \"usage\":null} \r\n"},
		{"event: delta\r\n\r\n", "event: delta\r\n\r\n"},
	} {
		assert.Equal(t, tc.want, string(r.Stream("mlx")([]byte(tc.input))))
	}
	chunk := "data: {\"choices\":[],\"usage\":{\"cost\":1}}\n\n: comment\ndata: malformed\r\ndata: [DONE]\n\n"
	assert.Equal(t, strings.Replace(chunk, `"cost":1`, "", 1), string(r.Stream("p")([]byte(chunk))))
}
