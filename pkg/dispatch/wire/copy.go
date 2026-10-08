package wire

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// CopyWithMetrics copies a streaming response with immediate flushing.
// When recorder is non-nil, it records TTFT on the first frame and
// extracts usage from the final frame. The optional transform function
// modifies frames before writing (e.g., injecting routing metadata into
// SSE usage lines).
//
// The copy is line-framed, not read-framed: a frame split across two
// reads is rejoined before anything observes, transforms or forwards it
// (see lineFramer). Both the observer and the transforms parse a line at
// a time, so without that the terminal usage frame could meter as zero
// tokens and the model-id rewrite could silently skip a frame. The cost
// is holding a partial trailing line until its newline arrives; whole
// frames are never delayed.
//
// If an SSE stream ends without a `data: [DONE]` terminator — either a
// graceful EOF or a mid-stream read error — this function synthesizes
// the sentinel before returning so OpenAI SDK iterators release instead
// of hanging on the dangling connection. Duplicate terminators are
// harmless: every major OpenAI SDK treats the first `[DONE]` as
// end-of-stream and discards later frames.
//
// NDJSON streams (Ollama's /api/* surface) get NO synthetic terminator —
// the Ollama protocol terminator is the `done: true` field on the last
// JSON object, and appending `data: [DONE]\n\n` would inject an
// unparsable line that crashes ollama-python's iter_lines+json.loads
// pipeline. The format is detected from w.Header()'s Content-Type;
// callers must set it before invoking the copy (CommitWithRouting does).
func CopyWithMetrics(w http.ResponseWriter, r io.Reader, recorder *llm.InferenceRecorder, transform func([]byte) []byte) {
	flusher, canFlush := w.(http.Flusher)
	if !canFlush && recorder == nil && transform == nil {
		_, _ = io.Copy(w, r)
		return
	}

	buf := make([]byte, StreamBufferSize)
	var framer lineFramer
	firstFrame := true
	var bestUsage UsageData
	selfTerminated := false
	clientGone := false

	// emit observes, transforms and forwards one whole-line frame,
	// reporting whether the client is still listening.
	emit := func(frame []byte) bool {
		// Observe the raw frame BEFORE transforming. The transform may
		// stamp cost+timing fields onto a terminal usage frame using
		// the live recorder; if we transformed first, the recorder
		// would still be empty when the closure reads it.
		if u, ok := observeChunk(recorder, frame); ok {
			bestUsage = bestUsage.merge(u)
		}

		// Every frame is offered to the transform, not just the ones
		// carrying usage. The usage-injection transform self-gates on
		// "usage" internally, so it is unaffected; a transform that
		// rewrites a field present on every frame -- the model id --
		// would otherwise only ever reach the terminal usage frame and
		// silently leave the rest of the stream untouched.
		if transform != nil {
			frame = transform(frame)
		}

		if _, writeErr := w.Write(frame); writeErr != nil {
			slog.Debug("Client disconnected during stream write", "error", writeErr)
			return false
		}
		if canFlush {
			flusher.Flush()
		}

		if bytes.Contains(frame, []byte("[DONE]")) || hasNamedEvent(frame) {
			selfTerminated = true
		}

		if recorder != nil && firstFrame {
			recorder.RecordFirstToken()
			firstFrame = false
		}
		return true
	}

	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if frame := framer.next(buf[:n]); len(frame) > 0 && !emit(frame) {
				clientGone = true
				break
			}
		}
		if readErr != nil {
			// Upstream is finished, so a last line that never got its
			// newline is complete as it stands. Release it before
			// judging how the stream ended.
			if tail := framer.flush(); len(tail) > 0 && !emit(tail) {
				clientGone = true
			}
			if !errors.Is(readErr, io.EOF) {
				slog.Error("Stream read error", "error", readErr)
				// Tag the recorder so the trailing RecordCompletion
				// emits with error_type. Mid-stream failures used to
				// emit cleanly as if successful, masking real upstream
				// breakage in dashboards. SetError unconditionally
				// overwrites -- safe here because the streaming path
				// only reaches this point on 2xx responses (the proxy
				// layer intercepts >=400 before stream-bytes copy
				// starts), so no prior status-derived tag exists to
				// clobber.
				if recorder != nil {
					recorder.SetError("upstream_stream_error", readErr.Error())
				}
			}
			break
		}
	}

	// Synthesize [DONE] if the upstream never sent one. Also skipped for
	// SSE that names its events (Anthropic Messages, the Responses API):
	// those protocols end on their own terminal event and do not know
	// [DONE], so the sentinel would be a stray frame. Skipped when the
	// client has already gone away -- writing to a dead socket would just
	// log another error. Also skipped for non-SSE streams (NDJSON):
	// `data: [DONE]\n\n` is unparsable JSON and crashes ollama-python's
	// iter_lines+json.loads on the trailing line.
	if !selfTerminated && !clientGone && isSSEContentType(w.Header().Get("Content-Type")) {
		SendSSEDone(w)
	}

	if recorder != nil {
		recorder.SetExtendedUsage(bestUsage.CachedTokens, bestUsage.ReasoningTokens, bestUsage.Cost, string(bestUsage.CostSource))
		recorder.RecordCompletion(bestUsage.In, bestUsage.Out)
	}
}

// isSSEContentType reports whether ct names an SSE stream. Treats the
// absent (empty) header as SSE so legacy callers that don't set
// Content-Type before calling CopyWithMetrics get the pre-fix
// behavior — only an explicit NDJSON content type opts out of [DONE]
// synthesis. RFC 6455 / 7230 say the type token is case-insensitive.
func isSSEContentType(ct string) bool {
	if ct == "" {
		return true
	}
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// hasNamedEvent reports whether frame carries an SSE `event:` field.
// OpenAI Chat streams are data-only; a named event marks a protocol with
// its own terminator.
func hasNamedEvent(frame []byte) bool {
	return bytes.HasPrefix(frame, []byte("event:")) || bytes.Contains(frame, []byte("\nevent:"))
}

// observeChunk feeds whole stream lines to the recorder: usage totals,
// the upstream's response model, and the reassembled reply text. Returns
// the usage the input carried, merged across its lines; callers merge it
// into their running total.
//
// Callers must pass line-framed bytes (lineFramer.next): every parse
// below is per-line, so a frame cut in half by a read boundary would be
// two pieces of invalid JSON and would contribute nothing.
//
// `model` is parsed independently of the usage gate: OpenAI SSE deltas
// carry `model` on every frame but `usage` only on the terminal one, so
// an upstream that errors out before sending usage still lands
// response.model on the recorder. The cheap string predicates keep the
// JSON parse off input that cannot contain either field.
func observeChunk(recorder *llm.InferenceRecorder, chunk []byte) (UsageData, bool) {
	if recorder == nil {
		return UsageData{}, false
	}

	var best UsageData
	found := false
	if bytes.Contains(chunk, []byte(`"usage"`)) || bytes.Contains(chunk, []byte(`"model"`)) {
		for line := range bytes.SplitSeq(chunk, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			u := ParseSSEFinalUsageExtended(line)
			if u.Model != "" {
				recorder.SetResponseModel(u.Model)
			}
			if u.In > 0 || u.Out > 0 {
				best, found = best.merge(u), true
				recorder.SetExtendedUsage(best.CachedTokens, best.ReasoningTokens, best.Cost, string(best.CostSource))
				// Stash tokensOut so the terminal-frame transform (which
				// runs immediately after) can compute tokens_per_second
				// from a populated snapshot.
				recorder.SetTokensOut(best.Out)
			}
		}
	}

	// Gated so a node with capture off pays no per-chunk parse.
	if llm.CaptureResponses() {
		recorder.AppendResponseText(StreamDeltaText(chunk))
	}
	return best, found
}
