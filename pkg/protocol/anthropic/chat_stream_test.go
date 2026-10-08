package anthropic

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sseEvent struct {
	Name string
	Data map[string]any
}

// translateStream runs engine bytes through a ChatStream in chunks of
// size step (0: all at once) and parses what it wrote.
func translateStream(t *testing.T, engine string, step int, closeAfter bool) []sseEvent {
	t.Helper()
	var out bytes.Buffer
	s := NewChatStream(&out, "qwen+agent")
	in := []byte(engine)
	if step == 0 {
		step = len(in)
	}
	for i := 0; i < len(in); i += step {
		_, err := s.Write(in[i:min(i+step, len(in))])
		require.NoError(t, err)
	}
	if closeAfter {
		require.NoError(t, s.Close())
	}
	return parseStreamEvents(t, out.String())
}

func parseStreamEvents(t *testing.T, output string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, frame := range strings.Split(strings.TrimSpace(output), "\n\n") {
		if frame == "" {
			continue
		}
		lines := strings.SplitN(frame, "\n", 2)
		require.Len(t, lines, 2, frame)
		var data map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data), frame)
		events = append(events, sseEvent{Name: strings.TrimPrefix(lines[0], "event: "), Data: data})
	}
	return events
}

func names(events []sseEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Name
	}
	return out
}

// A reasoning model's tool-using turn: thinking, text, then two tool
// calls whose arguments arrive in pieces, and a usage-only final chunk.
const toolStream = `data: {"id":"c1","choices":[{"delta":{"role":"assistant","reasoning_content":"need "}}]}

data: {"id":"c1","choices":[{"delta":{"reasoning_content":"files"}}]}

data: {"id":"c1","choices":[{"delta":{"content":"Reading."}}]}

data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read","arguments":"{\"fi"}}]}}]}

data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"le\":\"x\"}"}}]}}]}

data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"ls","arguments":"{}"}}]}}]}

data: {"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":20}}}

data: [DONE]

`

func TestChatStream_ToolTurn(t *testing.T) {
	for _, step := range []int{0, 1, 7} {
		events := translateStream(t, toolStream, step, false)
		require.Equal(t, []string{
			"message_start",
			"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
			"content_block_start", "content_block_delta", "content_block_stop",
			"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
			"content_block_start", "content_block_delta", "content_block_stop",
			"message_delta", "message_stop",
		}, names(events), "step %d", step)

		start := events[0].Data["message"].(map[string]any)
		assert.Equal(t, "msg_c1", start["id"])
		assert.Equal(t, "qwen+agent", start["model"], "the client's name")

		assert.Equal(t, map[string]any{"type": "thinking", "thinking": "", "signature": ""}, events[1].Data["content_block"])
		assert.Equal(t, "files", events[3].Data["delta"].(map[string]any)["thinking"])
		assert.Equal(t, "Reading.", events[6].Data["delta"].(map[string]any)["text"])
		assert.Equal(t, map[string]any{"type": "tool_use", "id": "call_a", "name": "read", "input": map[string]any{}}, events[8].Data["content_block"])
		assert.Equal(t, float64(2), events[8].Data["index"], "blocks are numbered in order")
		assert.Equal(t, `{"file":"x"}`, events[9].Data["delta"].(map[string]any)["partial_json"].(string)+events[10].Data["delta"].(map[string]any)["partial_json"].(string))
		assert.Equal(t, "call_b", events[12].Data["content_block"].(map[string]any)["id"])

		delta := events[15].Data
		assert.Equal(t, "tool_use", delta["delta"].(map[string]any)["stop_reason"])
		assert.Equal(t, map[string]any{"input_tokens": float64(30), "output_tokens": float64(9), "cache_read_input_tokens": float64(20)}, delta["usage"])
	}
}

// An engine that stops before the turn ends (no finish reason, no
// [DONE]) is reported as an error, not as a complete turn; one that
// finished needs no [DONE]; Close after [DONE] adds nothing.
func TestChatStream_CloseEndsACutStream(t *testing.T) {
	cut := `data: {"id":"c2","choices":[{"delta":{"content":"par"}}]}` + "\n\n" + `data: {"id":"c2","choices":[{"delta":{"content":"tial"}}]}`
	events := translateStream(t, cut, 0, true)
	assert.Equal(t, []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "error"}, names(events), "a final frame without its blank line still counts")
	assert.Equal(t, "api_error", events[5].Data["error"].(map[string]any)["type"])

	finished := `data: {"id":"c2","choices":[{"delta":{"content":"all"},"finish_reason":"stop"}]}`
	assert.Equal(t, "message_stop", lastName(translateStream(t, finished, 0, true)), "a finish reason ends the turn without [DONE]")

	once := translateStream(t, toolStream, 0, true)
	assert.Equal(t, "message_stop", lastName(once))
	assert.Equal(t, 1, strings.Count(strings.Join(names(once), " "), "message_stop"))

	assert.Equal(t, []string{"error"}, names(translateStream(t, "", 0, true)), "a silent engine did not answer")
}

func lastName(events []sseEvent) string { return events[len(events)-1].Name }

// Engines frame SSE with CRLF too, and a data field may span lines.
func TestChatStream_Framing(t *testing.T) {
	crlf := strings.ReplaceAll(toolStream, "\n", "\r\n")
	for _, step := range []int{0, 1, 5} {
		assert.Equal(t, names(translateStream(t, toolStream, 0, false)), names(translateStream(t, crlf, step, false)), "step %d", step)
	}
	split := "data: {\"id\":\"c\",\n" + `data: "choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"
	events := translateStream(t, split, 0, true)
	assert.Equal(t, "hi", events[2].Data["delta"].(map[string]any)["text"])
}

// Tool blocks carry all argument fragments, including ones preceding a name; a new id on a reused index is a new call; a turn with
// calls stops for tool_use whatever the engine called it.
func TestChatStream_ToolCallShapes(t *testing.T) {
	lateName := `data: {"id":"c","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"arguments":"{\"a\""}}]}}]}` + "\n\n" +
		`data: {"id":"c","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"grep","arguments":":1}"}}]}}]}` + "\n\n" +
		`data: {"id":"c","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	events := translateStream(t, lateName, 0, true)
	start := events[1].Data["content_block"].(map[string]any)
	assert.Equal(t, "grep", start["name"])
	assert.Equal(t, "call_x", start["id"])
	assert.Equal(t, `{"a":1}`, events[2].Data["delta"].(map[string]any)["partial_json"], "what waited for the name goes with it")
	assert.Equal(t, "tool_use", events[4].Data["delta"].(map[string]any)["stop_reason"], "calls stop for tool_use")

	sameIndex := `data: {"id":"c","choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"ls","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"id":"c","choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"cat","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"id":"c","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"
	events = translateStream(t, sameIndex, 0, true)
	var started []string
	for _, e := range events {
		if e.Name == "content_block_start" {
			started = append(started, e.Data["content_block"].(map[string]any)["name"].(string))
		}
	}
	assert.Equal(t, []string{"ls", "cat"}, started, "two calls, not one with joined arguments")

	nameless := `data: {"id":"c","choices":[{"delta":{"tool_calls":[{"index":0,"id":"z","function":{"arguments":"{}"}}]}}]}` + "\n\n" + `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	events = translateStream(t, nameless, 0, true)
	assert.Equal(t, "z", events[1].Data["content_block"].(map[string]any)["id"], "arguments are not lost for want of a name")
}

func TestChatStream_Errors(t *testing.T) {
	engineErr := `data: {"id":"c3","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"error":{"message":"context overflow","type":"server_error"}}` + "\n\n"
	events := translateStream(t, engineErr, 0, true)
	assert.Equal(t, []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "error"}, names(events))
	assert.Equal(t, map[string]any{"type": "api_error", "message": "context overflow"}, events[4].Data["error"])

	own := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
	events = translateStream(t, own, 0, true)
	assert.Equal(t, []string{"error"}, names(events), "zzRouter's own Anthropic error passes as it is")
	assert.Equal(t, "overloaded_error", events[0].Data["error"].(map[string]any)["type"])
}

func TestChatStream_InterleavedToolArguments(t *testing.T) {
	for _, middle := range []string{"", `data: {"choices":[{"delta":{"content":"Working","reasoning_content":"Think"}}]}` + "\n\n"} {
		engine := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{\"file\":"}},{"index":1,"id":"b","function":{"name":"read","arguments":"{\"file\":"}}]}}]}` + "\n\n" + middle +
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}},{"index":1,"function":{"arguments":"\"b.txt\"}"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
		for _, step := range []int{0, 1, 7} {
			events := translateStream(t, engine, step, true)
			calls := map[float64]string{}
			args := map[string]string{}
			closed := map[float64]bool{}
			for _, e := range events {
				index, _ := e.Data["index"].(float64)
				switch e.Name {
				case "content_block_start":
					block := e.Data["content_block"].(map[string]any)
					if block["type"] == "tool_use" {
						calls[index] = block["id"].(string)
					}
				case "content_block_delta":
					require.False(t, closed[index], "delta after block closed")
					delta := e.Data["delta"].(map[string]any)
					if delta["type"] == "input_json_delta" {
						args[calls[index]] += delta["partial_json"].(string)
					}
				case "content_block_stop":
					closed[index] = true
				}
			}
			require.Len(t, args, 2)
			assert.JSONEq(t, `{"file":"a.txt"}`, args["a"])
			assert.JSONEq(t, `{"file":"b.txt"}`, args["b"])
		}
	}
}

func TestChatStream_DoneWithoutFinishReasonIsTruncated(t *testing.T) {
	engine := `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\ndata: [DONE]\n\n"
	events := translateStream(t, engine, 0, true)
	assert.Equal(t, "error", events[len(events)-1].Name)
	assert.NotContains(t, names(events), "message_stop")
}

func TestChatStream_FirstToolStreamsBeforeCompletion(t *testing.T) {
	var out bytes.Buffer
	s := NewChatStream(&out, "agent")
	write := func(delta string) {
		t.Helper()
		_, err := s.Write([]byte("data: " + delta + "\n\n"))
		require.NoError(t, err)
	}
	write(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Write","arguments":"{\"content\":\"first"}},{"index":1,"id":"b","function":{"name":"Edit","arguments":"{\"content\":\"second"}}]}}]}`)
	first := parseStreamEvents(t, out.String())
	require.Equal(t, []string{"message_start", "content_block_start", "content_block_delta"}, names(first))
	assert.Equal(t, "a", first[1].Data["content_block"].(map[string]any)["id"])
	assert.Equal(t, `{"content":"first`, first[2].Data["delta"].(map[string]any)["partial_json"])

	write(`{"choices":[{"delta":{"content":"Working","reasoning_content":"Think"}}]}`)
	write(`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"}"}},{"index":0,"function":{"arguments":" part\"}"}}]}}]}`)
	beforeEnd := parseStreamEvents(t, out.String())
	require.Equal(t, []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta"}, names(beforeEnd))
	assert.Equal(t, ` part"}`, beforeEnd[3].Data["delta"].(map[string]any)["partial_json"])

	write(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	write(`[DONE]`)
	require.NoError(t, s.Close())
	events := parseStreamEvents(t, out.String())
	calls := map[float64]string{}
	args := map[string]string{}
	stops := map[float64]int{}
	var content []string
	for _, event := range events {
		index, _ := event.Data["index"].(float64)
		switch event.Name {
		case "content_block_start":
			block := event.Data["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				calls[index] = block["id"].(string)
			}
		case "content_block_delta":
			require.Zero(t, stops[index], "delta after closed block")
			delta := event.Data["delta"].(map[string]any)
			switch delta["type"] {
			case "input_json_delta":
				args[calls[index]] += delta["partial_json"].(string)
			case "thinking_delta":
				content = append(content, delta["thinking"].(string))
			case "text_delta":
				content = append(content, delta["text"].(string))
			}
		case "content_block_stop":
			stops[index]++
		}
	}
	require.Len(t, calls, 2)
	assert.JSONEq(t, `{"content":"first part"}`, args["a"])
	assert.JSONEq(t, `{"content":"second"}`, args["b"])
	assert.Equal(t, []string{"Think", "Working"}, content)
	for index := range calls {
		assert.Equal(t, 1, stops[index])
	}
	assert.Equal(t, "message_stop", lastName(events))
}

func TestChatStream_CapturedEngineTermination(t *testing.T) {
	for _, tc := range []struct {
		file string
		stop string
	}{
		{"chat_llamacpp_b10549.sse", "end_turn"},
		{"chat_mlx_0_31_3.sse", "max_tokens"},
		{"chat_vllm_0_29_0.sse", "max_tokens"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			capture, err := os.ReadFile("testdata/" + tc.file)
			require.NoError(t, err)
			for _, step := range []int{0, 1, 7} {
				for _, closeAfter := range []bool{false, true} {
					events := translateStream(t, string(capture), step, closeAfter)
					assert.NotContains(t, names(events), "error")
					require.Equal(t, "message_stop", lastName(events))
					assert.Equal(t, tc.stop, events[len(events)-2].Data["delta"].(map[string]any)["stop_reason"])
				}
			}
		})
	}
}
