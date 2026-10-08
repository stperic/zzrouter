package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// contextRoles are the roles Claude Code (2.1.154 on) puts in messages
// besides user and assistant, carrying the harness's context mid-
// conversation.
var contextRoles = map[string]bool{"system": true, "ctx": true, "msg": true}

// FoldContextTurns moves the text of every context-role turn into a user
// turn at the same place in the conversation: the user turn that follows
// it (after that turn's tool results, which the Messages API wants first),
// else the user turn just before it, else a new user turn of its own. Many
// chat templates refuse a system message after the first (Qwen3: "System
// message must be at the beginning"), and hoisting the text into the
// leading system prompt would change the prompt prefix on every turn and
// defeat the engine's prefix cache. Turns before the context stay
// byte-identical. A body with no context turn is returned as is.
func FoldContextTurns(body []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, refuse("body", "must be a JSON object: %v", err)
	}
	var messages []json.RawMessage
	if raw, ok := top["messages"]; ok {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, refuse("messages", "must be a list: %v", err)
		}
	}
	var out []foldTurn
	var pending []json.RawMessage
	// place puts the pending context into the turn just emitted when it is
	// a user turn, else into a new user turn after it.
	place := func() {
		if n := len(out); n > 0 && out[n-1].role == "user" {
			out[n-1].append(pending)
		} else {
			out = append(out, foldTurn{role: "user", blocks: pending})
		}
		pending = nil
	}
	folded := false
	for i, raw := range messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, refuse(fmt.Sprintf("messages.%d", i), "must be an object: %v", err)
		}
		switch {
		case contextRoles[m.Role]:
			texts, err := contextText(m.Content, i)
			if err != nil {
				return nil, err
			}
			pending = append(pending, texts...)
			folded = true
		case m.Role == "user" && pending != nil:
			blocks := rawBlocks(m.Content)
			at := leadingToolResults(blocks)
			merged := append(append(append([]json.RawMessage{}, blocks[:at]...), pending...), blocks[at:]...)
			out = append(out, foldTurn{role: "user", blocks: merged})
			pending = nil
		default:
			if pending != nil {
				place()
			}
			out = append(out, foldTurn{role: m.Role, raw: raw})
		}
	}
	if !folded {
		return body, nil
	}
	if pending != nil {
		place()
	}
	rendered := make([]json.RawMessage, len(out))
	for i, t := range out {
		if t.raw != nil {
			rendered[i] = t.raw
			continue
		}
		b, err := json.Marshal(map[string]any{"role": t.role, "content": t.blocks})
		if err != nil {
			return nil, refuse("messages", "cannot encode: %v", err)
		}
		rendered[i] = b
	}
	msgs, err := json.Marshal(rendered)
	if err != nil {
		return nil, refuse("messages", "cannot encode: %v", err)
	}
	top["messages"] = msgs
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(top); err != nil {
		return nil, refuse("body", "cannot encode: %v", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// foldTurn is one output turn: raw when unchanged, else role and blocks.
type foldTurn struct {
	raw    json.RawMessage
	role   string
	blocks []json.RawMessage
}

// append adds context blocks at the end of a user turn.
func (t *foldTurn) append(blocks []json.RawMessage) {
	if t.raw != nil {
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(t.raw, &m)
		t.blocks, t.raw = rawBlocks(m.Content), nil
	}
	t.blocks = append(t.blocks, blocks...)
}

// leadingToolResults counts the tool_result blocks a user turn opens with.
func leadingToolResults(blocks []json.RawMessage) int {
	for i, b := range blocks {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &probe) != nil || probe.Type != blockToolResult {
			return i
		}
	}
	return len(blocks)
}

// contextText returns a context turn's blocks, refusing anything but text.
func contextText(content json.RawMessage, idx int) ([]json.RawMessage, error) {
	blocks := rawBlocks(content)
	for j, b := range blocks {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &probe); err != nil || probe.Type != blockText {
			return nil, refuse(fmt.Sprintf("messages.%d.content.%d", idx, j), "a context message carries text, not %q", probe.Type)
		}
	}
	return blocks, nil
}

// rawBlocks reads content given as a string (one text block) or a list of
// blocks, keeping each block's bytes (cache_control included).
func rawBlocks(content json.RawMessage) []json.RawMessage {
	if len(bytes.TrimSpace(content)) == 0 || string(content) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		b, _ := json.Marshal(map[string]string{"type": blockText, "text": s})
		return []json.RawMessage{b}
	}
	var blocks []json.RawMessage
	_ = json.Unmarshal(content, &blocks)
	return blocks
}
