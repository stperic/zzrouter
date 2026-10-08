package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// A Chat Completions stream chunk as the translation reads it.
type chatChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content          *string        `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			Reasoning        string         `json:"reasoning"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
	// An engine reports a failure mid-stream as an OpenAI error object.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	// zzRouter's own in-band streamer already speaks Anthropic: an error
	// event it writes carries type "error".
	Type string `json:"type"`
}

// sseDone ends a Chat Completions stream.
const sseDone = "[DONE]"

// truncatedMessage is what a client is told when the engine's stream
// stopped before the turn did; once the stream is open, an error event
// is the only way left to say so.
const truncatedMessage = "the engine's stream ended before the turn did"

// streamCall tracks a tool call. Later calls wait while the first streams,
// because a Messages block cannot reopen after it closes.
type streamCall struct {
	index   int
	id      string
	name    string
	pending strings.Builder
	opened  bool
}

type streamContent struct {
	kind string
	text string
}

// ChatStream translates a Chat Completions SSE stream into a Messages
// SSE stream. Write takes the engine's bytes in any chunking; Close ends
// the Messages stream if the engine did not. It is not safe for
// concurrent use.
type ChatStream struct {
	out   io.Writer
	model string
	buf   []byte

	started  bool
	finished bool
	// ended is set by a finish reason; DONE alone may be synthesized on EOF.
	ended bool
	// open is the kind of the block being streamed ("" when none),
	// openCall the call it belongs to, and index the Messages index of
	// the next block.
	open     string
	openCall *streamCall
	index    int
	calls    map[int]*streamCall
	ordered  []*streamCall
	deferred []streamContent
	stop     string
	usage    Usage
	err      error
}

// NewChatStream writes the translation of an engine's stream to out,
// answering as model (the name the client used).
func NewChatStream(out io.Writer, model string) *ChatStream {
	return &ChatStream{out: out, model: model, calls: map[int]*streamCall{}, stop: stopEndTurn}
}

// Write consumes engine bytes; complete SSE frames are translated as
// they arrive.
func (s *ChatStream) Write(p []byte) (int, error) {
	// SSE allows CRLF framing; a "\r" left at the end waits for its "\n".
	s.buf = bytes.ReplaceAll(append(s.buf, p...), []byte("\r\n"), []byte("\n"))
	for {
		end := bytes.Index(s.buf, []byte("\n\n"))
		if end < 0 {
			break
		}
		frame := s.buf[:end]
		s.buf = s.buf[end+2:]
		s.frame(frame)
	}
	return len(p), s.err
}

// Close ends the Messages stream. One the engine cut short ends with an
// error event, not as if the turn were complete.
func (s *ChatStream) Close() error {
	if len(bytes.TrimSpace(s.buf)) > 0 {
		s.frame(s.buf)
		s.buf = nil
	}
	if !s.ended && !s.finished {
		s.flushCalls()
		s.closeBlock()
		s.emit("error", envelope(ErrorTypeAPI, truncatedMessage))
		s.finished = true
	}
	s.finish()
	return s.err
}

// frame handles one SSE frame: its data lines, joined.
func (s *ChatStream) frame(frame []byte) {
	if s.finished {
		return
	}
	var lines [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			lines = append(lines, bytes.TrimPrefix(rest, []byte(" ")))
		}
	}
	if len(lines) == 0 {
		return
	}
	data := bytes.TrimSpace(bytes.Join(lines, []byte("\n")))
	if string(data) == sseDone {
		// The proxy may synthesize DONE on EOF; only a finish reason completes a turn.
		if s.ended {
			s.finish()
		}
		return
	}
	var chunk chatChunk
	if json.Unmarshal(data, &chunk) != nil {
		return
	}
	switch {
	case chunk.Type == "error":
		s.closeBlock()
		s.raw("error", data)
		s.finished = true
	case chunk.Error != nil:
		s.closeBlock()
		s.emit("error", envelope(ErrorTypeAPI, chunk.Error.Message))
		s.finished = true
	default:
		s.chunk(chunk)
	}
}

func (s *ChatStream) chunk(c chatChunk) {
	s.begin(c.ID)
	if c.Usage != nil {
		s.usage = c.Usage.messagesUsage()
	}
	if len(c.Choices) == 0 {
		return
	}
	choice := c.Choices[0]
	d := choice.Delta
	if thinking := firstNonEmpty(d.ReasoningContent, d.Reasoning); thinking != "" {
		s.contentDelta(blockThinking, thinking)
	}
	if d.Content != nil && *d.Content != "" {
		s.contentDelta(blockText, *d.Content)
	}
	for _, delta := range d.ToolCalls {
		s.toolDelta(delta)
	}
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		s.stop = stopReason(*choice.FinishReason)
		s.ended = true
	}
}

// toolDelta streams one piece of a tool call. A call is its index,
// unless a delta brings a different id on the same index, which some
// engines send for every call: that starts the next call.
func (s *ChatStream) toolDelta(d chatToolCall) {
	call := s.calls[d.Index]
	if call == nil || (d.ID != "" && call.id != "" && d.ID != call.id) {
		call = &streamCall{index: len(s.ordered)}
		s.calls[d.Index] = call
		s.ordered = append(s.ordered, call)
	}
	if call.id == "" {
		call.id = d.ID
	}
	if call.name == "" {
		call.name = d.Function.Name
	}
	call.pending.WriteString(d.Function.Arguments)
	if call == s.ordered[0] && call.id != "" && call.name != "" {
		if !call.opened {
			s.startCall(call)
		} else if call.pending.Len() > 0 {
			s.delta(map[string]any{"type": "input_json_delta", "partial_json": call.pending.String()})
			call.pending.Reset()
		}
	}
}

// contentDelta keeps the streaming tool open if text or thinking is interleaved.
func (s *ChatStream) contentDelta(kind, text string) {
	if len(s.ordered) > 0 && s.ordered[0].opened {
		s.deferred = append(s.deferred, streamContent{kind: kind, text: text})
		return
	}
	s.emitContent(kind, text)
}

func (s *ChatStream) emitContent(kind, text string) {
	if kind == blockThinking {
		s.ensureBlock(kind, map[string]any{"type": kind, "thinking": "", "signature": ""})
		s.delta(map[string]any{"type": "thinking_delta", "thinking": text})
		return
	}
	s.ensureBlock(kind, map[string]any{"type": kind, "text": ""})
	s.delta(map[string]any{"type": "text_delta", "text": text})
}

// startCall opens a call's block and sends the arguments that waited for
// its name.
func (s *ChatStream) startCall(call *streamCall) {
	s.closeBlock()
	call.opened = true
	s.openBlock(blockToolUse, call, map[string]any{
		"type": blockToolUse, "id": toolUseID(call.id, call.index), "name": call.name, "input": map[string]any{},
	})
	if call.pending.Len() > 0 {
		s.delta(map[string]any{"type": "input_json_delta", "partial_json": call.pending.String()})
		call.pending.Reset()
	}
}

// flushCalls emits waiting calls, then deferred text and reasoning, without
// closing a tool before all its argument fragments have arrived.
func (s *ChatStream) flushCalls() {
	for _, call := range s.ordered {
		if !call.opened {
			s.startCall(call)
		}
	}
	for _, content := range s.deferred {
		s.emitContent(content.kind, content.text)
	}
	s.deferred = nil
}

// begin opens the Messages stream on the engine's first chunk.
func (s *ChatStream) begin(chatID string) {
	if s.started {
		return
	}
	s.started = true
	s.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": messageID(chatID), "type": "message", "role": "assistant", "model": s.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": Usage{},
	}})
}

// ensureBlock keeps streaming into the open block of kind, or opens one.
func (s *ChatStream) ensureBlock(kind string, start map[string]any) {
	if s.open == kind && s.openCall == nil {
		return
	}
	s.closeBlock()
	s.openBlock(kind, nil, start)
}

func (s *ChatStream) openBlock(kind string, call *streamCall, start map[string]any) {
	s.open, s.openCall = kind, call
	s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": s.index, "content_block": start})
}

func (s *ChatStream) delta(d map[string]any) {
	s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index, "delta": d})
}

func (s *ChatStream) closeBlock() {
	if s.open == "" {
		return
	}
	s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index})
	s.open, s.openCall = "", nil
	s.index++
}

// finish ends the Messages stream once: the open block, how the turn
// stopped and what it used, then message_stop.
func (s *ChatStream) finish() {
	if s.finished {
		return
	}
	s.begin("")
	s.flushCalls()
	s.closeBlock()
	s.emit("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopWithTools(s.stop, len(s.ordered) > 0), "stop_sequence": nil}, "usage": s.usage})
	s.emit("message_stop", map[string]any{"type": "message_stop"})
	s.finished = true
}

func (s *ChatStream) emit(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		s.err = fmt.Errorf("encode %s event: %w", event, err)
		return
	}
	s.raw(event, data)
}

func (s *ChatStream) raw(event string, data []byte) {
	if s.err != nil {
		return
	}
	_, s.err = fmt.Fprintf(s.out, "event: %s\ndata: %s\n\n", event, data)
}
