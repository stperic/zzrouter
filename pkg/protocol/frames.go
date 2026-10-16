package protocol

import "bytes"

// RewriteStreamJSON preserves framing while applying a payload editor to SSE or NDJSON lines.
func RewriteStreamJSON(chunk []byte, rewrite func([]byte) []byte) []byte {
	lines := bytes.Split(chunk, []byte("\n"))
	modified := false
	for i, line := range lines {
		payload, at := StreamFrameJSON(line)
		if payload == nil {
			continue
		}
		rewritten := rewrite(payload)
		// Equality preserves malformed fragments and no-op formatting.
		if rewritten == nil || bytes.Equal(rewritten, payload) {
			continue
		}
		// Preserve indentation and trailing CR around changed payloads.
		out := make([]byte, 0, len(line)-len(payload)+len(rewritten))
		out = append(out, line[:at]...)
		out = append(out, rewritten...)
		lines[i] = append(out, line[at+len(payload):]...)
		modified = true
	}
	if !modified {
		return chunk
	}
	return bytes.Join(lines, []byte("\n"))
}

// frameSpace excludes newlines so line boundaries survive payload replacement.
const frameSpace = " \t\r"

// StreamFrameJSON retains offsets so payload edits preserve the surrounding wire bytes.
func StreamFrameJSON(line []byte) (payload []byte, at int) {
	lead := len(line) - len(bytes.TrimLeft(line, frameSpace))
	body := bytes.TrimRight(line[lead:], frameSpace)

	if bytes.Equal(body, []byte("data: [DONE]")) {
		return nil, 0
	}
	if rest, ok := bytes.CutPrefix(body, []byte("data: ")); ok {
		return rest, lead + len("data: ")
	}
	// Ollama frames lack an SSE prefix.
	if len(body) > 0 && body[0] == '{' {
		return body, lead
	}
	return nil, 0
}
