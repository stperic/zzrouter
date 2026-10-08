package wire

import (
	"bytes"
	"encoding/json"
	"strings"
)

// responseShape covers every reply body zzRouter proxies: OpenAI chat
// (choices[].message), OpenAI streaming (choices[].delta), OpenAI
// completions (choices[].text), Ollama chat (message) and Ollama
// generate (response). One decode handles all of them because the field
// sets do not collide.
type responseShape struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Text string `json:"text"`
	} `json:"choices"`
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Response string `json:"response"`
}

// text returns the first populated assistant field.
func (r responseShape) text() string {
	for _, c := range r.Choices {
		switch {
		case c.Message.Content != "":
			return c.Message.Content
		case c.Delta.Content != "":
			return c.Delta.Content
		case c.Text != "":
			return c.Text
		}
	}
	if r.Message.Content != "" {
		return r.Message.Content
	}
	return r.Response
}

// ResponseText extracts the assistant's reply from a complete,
// non-streaming response body. Returns "" for bodies that carry no
// assistant text, including error envelopes.
func ResponseText(body []byte) string {
	var shape responseShape
	if err := json.Unmarshal(body, &shape); err != nil {
		return ""
	}
	return shape.text()
}

// StreamDeltaText extracts the assistant text carried by one streamed
// chunk. A chunk may hold several SSE frames or Ollama NDJSON lines, so
// every line contributes and the caller appends the result in order.
func StreamDeltaText(chunk []byte) string {
	var b strings.Builder
	for line := range bytes.SplitSeq(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		line = bytes.TrimPrefix(line, []byte("data:"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		var shape responseShape
		if err := json.Unmarshal(line, &shape); err != nil {
			continue
		}
		b.WriteString(shape.text())
	}
	return b.String()
}
