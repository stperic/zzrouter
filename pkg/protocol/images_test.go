package protocol

import "testing"

func TestHasImages(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"chat", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, true},
		{"responses", `{"input":[{"type":"message","content":[{"type":"input_image","image_url":"x"}]}]}`, true},
		{"responses implicit message", `{"input":[{"role":"user","content":[{"type":"input_image"}]}]}`, true},
		{"messages", `{"messages":[{"content":[{"type":"image","source":{"type":"url","url":"x"}}]}]}`, true},
		{"tool result", `{"messages":[{"content":[{"type":"tool_result","content":[{"type":"image"}]}]}]}`, true},
		{"ollama chat", `{"messages":[{"content":"describe","images":["AAA"]}]}`, true},
		{"ollama generate", `{"images":["AAA"]}`, true},
		{"empty", `{"images":[],"messages":[{"images":[],"content":"image_url"}]}`, false},
		{"text", `{"input":"input_image","messages":[{"content":[{"type":"text","text":"image"}]}]}`, false},
		{"schema", `{"tools":[{"input_schema":{"type":"image"}}],"metadata":{"images":["x"]}}`, false},
		{"tool json", `{"messages":[{"content":[{"type":"tool_use","input":{"type":"image"}}]}]}`, false},
		{"response function", `{"input":[{"type":"function_call","content":[{"type":"input_image"}]}]}`, false},
		{"malformed", `{"messages":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasImages([]byte(tc.body)); got != tc.want {
				t.Fatalf("HasImages = %v, want %v", got, tc.want)
			}
		})
	}
}
