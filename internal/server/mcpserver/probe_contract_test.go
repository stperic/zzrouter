package mcpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Synthetic responses exercise evidence, not a model family's declared capabilities.
func TestEmbeddingProbeContract(t *testing.T) {
	for _, tc := range []struct {
		name, vector string
		valid        bool
	}{{"finite", "[0.25,-0.5]", true}, {"empty", "[]", false}, {"null", "null", false}, {"null element", "[null]", false}, {"text", `["0.5"]`, false}, {"overflow", "[1e999]", false}} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/v1/embeddings", r.URL.Path)
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, "m@worker", body["model"])
				require.IsType(t, "", body["input"])
				require.NotEmpty(t, body["input"])
				require.NotContains(t, body, "messages")
				writeFixture(t, w, []byte(`{"model":"m","data":[{"index":0,"embedding":`+tc.vector+`}],"usage":{"prompt_tokens":3,"total_tokens":3}}`))
			})
			result := callAgentTool(t, d, "zz_chat", map[string]any{"model": "m", "node": "worker", "probe": "embed"})
			if !tc.valid {
				require.Equal(t, true, result["isError"], result)
				problem, ok := result["structuredContent"].(map[string]any)
				require.True(t, ok, result)
				require.EqualValues(t, 502, problem["status"])
				return
			}
			require.NotEqual(t, true, result["isError"], result)
			fields := result["structuredContent"].(map[string]any)
			require.Equal(t, "embed", fields["probe"])
			require.Equal(t, "verified", fields["verdict"])
			require.EqualValues(t, 2, fields["dimensions"])
			require.NotContains(t, fields, "embedding")
			require.NotContains(t, fields, "data")
			require.NotEmpty(t, fields["next_steps"])
		})
	}
}

func TestVisionProbeContract(t *testing.T) {
	for _, tc := range []struct {
		name, behavior, want string
	}{{"image evidence", "pixels", "verified"}, {"image ignored", "red", "inconclusive"}, {"text only", "text", "inconclusive"}, {"truncated", "length", "inconclusive"}} {
		t.Run(tc.name, func(t *testing.T) {
			images := map[string]bool{}
			calls := 0
			d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, "/v1/chat/completions", r.URL.Path)
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.EqualValues(t, 16, body["max_tokens"])
				require.Equal(t, false, body["stream"])
				message := body["messages"].([]any)[0].(map[string]any)
				parts := message["content"].([]any)
				require.Len(t, parts, 2)
				question := parts[0].(map[string]any)["text"].(string)
				require.NotContains(t, strings.ToLower(question), "red")
				require.NotContains(t, strings.ToLower(question), "blue")
				url := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
				require.True(t, strings.HasPrefix(url, "data:image/png;base64,"))
				raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/png;base64,"))
				require.NoError(t, err)
				img, err := png.Decode(bytes.NewReader(raw))
				require.NoError(t, err)
				require.LessOrEqual(t, img.Bounds().Dx(), 32)
				rgbR, _, rgbB, _ := img.At(0, 0).RGBA()
				answer := "blue"
				if rgbR > rgbB {
					answer = "red"
				}
				images[answer] = true
				finish := "stop"
				switch tc.behavior {
				case "red":
					answer = "red"
				case "text":
					answer = "I can read the question."
				case "length":
					finish = "length"
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"model": "m", "choices": []any{map[string]any{"finish_reason": finish, "message": map[string]any{"content": answer}}}}))
			})
			result := callAgentTool(t, d, "zz_chat", map[string]any{"model": "m", "probe": "vision", "message": "red", "max_tokens": 9999})
			require.NotEqual(t, true, result["isError"], result)
			fields := result["structuredContent"].(map[string]any)
			require.Equal(t, tc.want, fields["verdict"])
			require.Equal(t, 2, calls)
			require.Len(t, images, 2)
		})
	}
}

func TestEmbeddingUsageContract(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		valid       bool
	}{
		{"absent", "", true},
		{"extra", `,"usage":{"prompt_tokens":2,"total_tokens":2,"provider_extra":[1,2,3]}`, true},
		{"malformed", `,"usage":{"total_tokens":"invalid"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
				writeFixture(t, w, []byte(`{"model":"m","data":[{"embedding":[0.5]}]`+tc.usage+`}`))
			})
			result := callAgentTool(t, d, "zz_chat", map[string]any{"model": "m", "probe": "embed"})
			if !tc.valid {
				require.Equal(t, true, result["isError"], result)
				require.EqualValues(t, 502, result["structuredContent"].(map[string]any)["status"])
				return
			}
			require.NotEqual(t, true, result["isError"], result)
			fields := result["structuredContent"].(map[string]any)
			require.Equal(t, "verified", fields["verdict"])
			usage, ok := fields["usage"].(map[string]any)
			require.True(t, ok, fields)
			require.NotContains(t, usage, "provider_extra")
		})
	}
}

func TestChatReplyEvidenceContract(t *testing.T) {
	for _, tc := range []struct{ name, reply, finish, verdict string }{
		{"answer", "hello", "stop", "verified"},
		{"empty", "", "stop", "inconclusive"},
		{"whitespace", "  ", "stop", "inconclusive"},
		{"truncated", "hello", "length", "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"model": "m", "choices": []any{map[string]any{"finish_reason": tc.finish, "message": map[string]any{"content": tc.reply}}}}))
			})
			result := callAgentTool(t, d, "zz_chat", map[string]any{"model": "m", "message": "hello"})
			require.NotEqual(t, true, result["isError"], result)
			require.Equal(t, tc.verdict, result["structuredContent"].(map[string]any)["verdict"])
		})
	}
}

func TestProbeRefusalContract(t *testing.T) {
	for _, tc := range []struct {
		code             string
		status           int
		supportedRefusal bool
	}{
		{"unsupported_input", 400, true}, {"tool_calling_not_enabled", 400, true},
		{"chat_template_rejected", 400, false}, {"unauthorized", 401, false}, {"upstream_timeout", 504, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			// Typed OpenAI error code, with deliberately misleading prose.
			d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": tc.code, "type": "invalid_request_error", "message": "vision is not supported"}}))
			})
			result := callAgentTool(t, d, "zz_chat", map[string]any{"model": "m", "probe": "tools"})
			require.Equal(t, true, result["isError"])
			problem, ok := result["structuredContent"].(map[string]any)
			require.True(t, ok, result)
			require.Equal(t, tc.code, problem["code"])
			require.EqualValues(t, tc.status, problem["status"])
			if tc.supportedRefusal {
				require.Equal(t, "not_supported", problem["verdict"])
			} else {
				require.NotContains(t, problem, "verdict")
			}
		})
	}
}
