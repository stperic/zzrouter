package wire

import (
	"bytes"
	"encoding/json"
)

// contentBearing reports whether one stream line carries generated content: text, reasoning,
// tool-call arguments or a legacy completion text. Role-only deltas, SSE comments, Anthropic
// message_start and content_block_start, [DONE] and usage-only chunks are not content, so they
// must not stamp time to first token.
func contentBearing(line []byte) bool {
	payload := bytes.TrimSpace(line)
	if bytes.HasPrefix(payload, []byte("event:")) {
		return false
	}
	if data, ok := bytes.CutPrefix(payload, []byte("data:")); ok {
		payload = bytes.TrimSpace(data)
	}
	if len(payload) == 0 || payload[0] != '{' {
		return false
	}
	// Cheap gate: every shape below carries one of these keys.
	if !bytes.Contains(payload, []byte(`"choices"`)) &&
		!bytes.Contains(payload, []byte(`"type"`)) &&
		!bytes.Contains(payload, []byte(`"message"`)) &&
		!bytes.Contains(payload, []byte(`"response"`)) {
		return false
	}
	var frame map[string]json.RawMessage
	if json.Unmarshal(payload, &frame) != nil {
		return false
	}
	var kind string
	_ = json.Unmarshal(frame["type"], &kind)
	switch kind {
	case "content_block_delta",
		"response.output_text.delta",
		"response.reasoning_text.delta",
		"response.function_call_arguments.delta":
		return true
	}
	if raw, ok := frame["choices"]; ok && choicesCarryContent(raw) {
		return true
	}
	if raw, ok := frame["message"]; ok && ollamaMessageCarriesContent(raw) {
		return true
	}
	var response string
	_ = json.Unmarshal(frame["response"], &response)
	return response != ""
}

// choicesCarryContent reads OpenAI chat deltas and legacy completion texts.
func choicesCarryContent(raw json.RawMessage) bool {
	var choices []struct {
		Text  string `json:"text"`
		Delta struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &choices) != nil {
		return false
	}
	for _, c := range choices {
		d := c.Delta
		if c.Text != "" || d.Content != "" || d.ReasoningContent != "" || d.Reasoning != "" || nonEmptyArray(d.ToolCalls) {
			return true
		}
	}
	return false
}

// ollamaMessageCarriesContent reads the Ollama /api/chat NDJSON message object.
func ollamaMessageCarriesContent(raw json.RawMessage) bool {
	var message struct {
		Content   string          `json:"content"`
		Thinking  string          `json:"thinking"`
		ToolCalls json.RawMessage `json:"tool_calls"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return false
	}
	return message.Content != "" || message.Thinking != "" || nonEmptyArray(message.ToolCalls)
}

func nonEmptyArray(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte("[]"))
}
