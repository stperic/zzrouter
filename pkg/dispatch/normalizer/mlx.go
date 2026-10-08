// MLX reasoning→content normalizer.
//
// mlx_lm's OpenAI-compatible server emits chat completions with a `reasoning`
// field instead of `content` when serving reasoning models. Stock OpenAI SDKs
// only read `content`, so clients see empty assistant turns.
//
// The normalizer copies `reasoning` into `content` (without touching
// `reasoning`, so clients that want chain-of-thought display still get it).
// We apply the fix at two sites:
//
//   - message.content    in non-streaming responses
//   - delta.content      in streaming responses
//
// The stream normalizer is intentionally conservative: it only runs when a
// chunk already contains `"reasoning"`, and preserves framing by scanning
// line-by-line.
package normalizer

import (
	"bytes"
	"encoding/json"
)

// RegisterMLX installs the MLX reasoning→content shim on the given Registry.
// Called during server wiring.
func RegisterMLX(r *Registry) {
	r.RegisterBody("mlx", NormalizeMLXBody)
	r.RegisterStream("mlx", NormalizeMLXStream)
}

// NormalizeMLXBody rewrites a non-streaming chat completion so that each
// choice's message.content is populated from message.reasoning when
// content is missing or empty. Returns the input unchanged if the body is
// not a chat completion or doesn't need rewriting.
func NormalizeMLXBody(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"reasoning"`)) {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return body
	}
	changed := false
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		if copyReasoningIntoContent(msg) {
			changed = true
		}
	}
	if !changed {
		return body
	}
	result, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return result
}

// NormalizeMLXStream rewrites each `data: {...}` SSE line in a chunk so
// that every delta.content is populated from delta.reasoning when content
// is missing or empty. Preserves framing by editing in place at the line
// level.
func NormalizeMLXStream(chunk []byte) []byte {
	if !bytes.Contains(chunk, []byte(`"reasoning"`)) {
		return chunk
	}
	lines := bytes.Split(chunk, []byte("\n"))
	modified := false
	for i, line := range lines {
		trimmed := bytes.TrimLeft(line, " \t")
		if !bytes.HasPrefix(trimmed, []byte("data: ")) {
			continue
		}
		payload := trimmed[6:]
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(payload, &obj); err != nil {
			continue
		}
		choices, ok := obj["choices"].([]any)
		if !ok {
			continue
		}
		chunkChanged := false
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			// Streaming uses "delta"; defensive fallback to "message" for
			// providers that occasionally emit the non-streaming shape
			// mid-stream.
			if delta, ok := choice["delta"].(map[string]any); ok {
				if copyReasoningIntoContent(delta) {
					chunkChanged = true
				}
			}
			if msg, ok := choice["message"].(map[string]any); ok {
				if copyReasoningIntoContent(msg) {
					chunkChanged = true
				}
			}
		}
		if !chunkChanged {
			continue
		}
		result, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		// Preserve any leading whitespace before "data: " so indentation-
		// sensitive clients still see the original framing.
		prefix := line[:len(line)-len(trimmed)]
		lines[i] = append(append([]byte{}, prefix...), append([]byte("data: "), result...)...)
		modified = true
	}
	if !modified {
		return chunk
	}
	return bytes.Join(lines, []byte("\n"))
}

// copyReasoningIntoContent mutates msg so that content is populated from
// reasoning when content is missing or empty. Leaves reasoning intact.
// Returns true if msg was modified.
func copyReasoningIntoContent(msg map[string]any) bool {
	reasoning, ok := msg["reasoning"].(string)
	if !ok || reasoning == "" {
		return false
	}
	if existing, ok := msg["content"].(string); ok && existing != "" {
		return false
	}
	msg["content"] = reasoning
	return true
}
