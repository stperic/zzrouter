package wire

import (
	"bytes"
	"errors"
	"io"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// streamMeter observes a stream's raw frames for usage, TTFT and its terminal frame.
type streamMeter struct {
	framer   lineFramer
	best     UsageData
	recorder *llm.InferenceRecorder
	first    bool
	done     bool
	failed   bool
	finished bool
	midLine  bool
	result   Result
}

func (m *streamMeter) next(chunk []byte) []byte {
	frame := m.framer.next(chunk)
	m.observe(frame)
	return frame
}

func (m *streamMeter) tail() []byte {
	frame := m.framer.flush()
	if len(frame) > 0 {
		m.midLine = frame[len(frame)-1] != '\n'
	}
	m.observe(frame)
	return frame
}

func (m *streamMeter) observe(frame []byte) {
	if m.finished || len(frame) == 0 {
		return
	}
	usageSeen := false
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		if !m.first && contentBearing(line) {
			m.recorder.RecordFirstToken()
			m.first = true
		}
		done, failed := terminal(line)
		m.done = m.done || done
		m.failed = m.failed || failed
		if u, ok := observeChunk(m.recorder, line); ok {
			m.best = m.best.merge(u)
			usageSeen = true
		}
	}
	if usageSeen {
		m.recorder.SetExtendedUsage(m.best.CachedTokens, m.best.ReasoningTokens, m.best.Cost, string(m.best.CostSource))
		m.recorder.SetTokensOut(m.best.Out)
	}
}

func (m *streamMeter) finish(readErr error, clientGone bool) Result {
	if m.finished {
		return m.result
	}
	m.tail()
	m.result = Result{Outcome: Truncated, MidLine: m.midLine}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		m.result.Err = readErr
	}
	switch {
	case clientGone:
		m.result.Outcome = ClientGone
	case m.failed:
		m.result.Outcome = UpstreamFailed
		m.recorder.SetError("upstream_stream_failed", "The upstream stream reported an error.")
	// The terminal frame is the proof; a reset after it loses nothing.
	case m.done:
		m.result.Outcome = Completed
	case m.result.Err != nil:
		m.recorder.SetError("upstream_stream_error", readErr.Error())
	default:
		m.recorder.SetError("upstream_stream_truncated", "The upstream stream ended before completing.")
	}
	m.finished = true
	m.recorder.SetExtendedUsage(m.best.CachedTokens, m.best.ReasoningTokens, m.best.Cost, string(m.best.CostSource))
	m.recorder.RecordCompletion(m.best.In, m.best.Out)
	return m.result
}

func observeChunk(recorder *llm.InferenceRecorder, frame []byte) (UsageData, bool) {
	if recorder == nil {
		return UsageData{}, false
	}
	var usage UsageData
	if bytes.Contains(frame, []byte(`"usage"`)) || bytes.Contains(frame, []byte(`"model"`)) {
		usage = ParseSSEFinalUsageExtended(bytes.TrimSpace(frame))
		if usage.Model != "" {
			recorder.SetResponseModel(usage.Model)
		}
	}
	// Gated so a node with capture off pays no per-frame parse.
	if llm.CaptureResponses() {
		recorder.AppendResponseText(StreamDeltaText(frame))
	}
	return usage, usage.In > 0 || usage.Out > 0
}
