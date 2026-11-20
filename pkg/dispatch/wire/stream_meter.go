package wire

import (
	"bytes"
	"errors"
	"io"
	"time"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// streamMeter observes a stream's raw frames for usage, TTFT and its terminal frame.
type streamMeter struct {
	framer      lineFramer
	best        UsageData
	observer    StreamObserver
	stopContent bool
	done        bool
	failed      bool
	finished    bool
	midLine     bool
	result      Result
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
		if m.observer != nil && m.observeLine(line) {
			usageSeen = true
		}
		done, failed := terminal(line)
		m.done = m.done || done
		m.failed = m.failed || failed
	}
	if usageSeen {
		m.observer.Usage(m.best)
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
	// The terminal frame is the proof; a reset after it loses nothing.
	case m.done:
		m.result.Outcome = Completed
	}
	m.finished = true
	if m.observer != nil {
		m.observer.End(m.result, m.best)
	}
	return m.result
}

func (m *streamMeter) observeLine(frame []byte) bool {
	if !m.stopContent && contentBearing(frame) {
		m.stopContent = !m.observer.ContentToken()
	}
	var usage UsageData
	measureCounts := m.observer.MeasureTokenPresence()
	if streamUsageCandidate(frame, measureCounts) {
		usage = ParseSSEFinalUsageExtended(frame)
		if usage.Model != "" {
			m.observer.SetResponseModel(usage.Model)
		}
	}
	// Gated so a node with capture off pays no per-frame parse.
	if m.observer.CaptureText() {
		m.observer.AppendResponseText(StreamDeltaText(frame))
	}
	reported := usage.recordable()
	if measureCounts {
		reported = reported || usage.HasInput || usage.HasOutput
	}
	if reported {
		m.best = m.best.merge(usage)
	}
	return reported
}

func streamUsageCandidate(frame []byte, measureCounts bool) bool {
	if bytes.Contains(frame, []byte(`"usage"`)) || bytes.Contains(frame, []byte(`"model"`)) {
		return true
	}
	return measureCounts && (bytes.Contains(frame, []byte(`"eval_count"`)) ||
		bytes.Contains(frame, []byte(`"prompt_eval_count"`)))
}

// StreamObserver receives raw stream observations before any response rewrite.
type StreamObserver interface {
	ContentToken() bool // Return true to keep observing later content.
	SetResponseModel(string)
	CaptureText() bool
	AppendResponseText(string)
	MeasureTokenPresence() bool
	Usage(UsageData)
	End(Result, UsageData)
}

type recorderObserver struct{ *llm.InferenceRecorder }

// RecorderObserver publishes stream observations through the inference recorder.
func RecorderObserver(rec *llm.InferenceRecorder) StreamObserver {
	if rec == nil {
		return nil
	}
	return recorderObserver{rec}
}

func (o recorderObserver) ContentToken() bool {
	o.RecordFirstToken()
	return false
}

func (recorderObserver) CaptureText() bool { return llm.CaptureResponses() }

func (recorderObserver) MeasureTokenPresence() bool { return false }

func (o recorderObserver) Usage(u UsageData) {
	o.SetExtendedUsage(u.CachedTokens, u.ReasoningTokens, u.Cost, string(u.CostSource))
	o.SetTokensOut(u.Out)
}

func (o recorderObserver) End(result Result, u UsageData) {
	// Calibration uses Result.Failure's wire code, while the recorder distinguishes truncation from read errors.
	switch {
	case result.Outcome == UpstreamFailed:
		o.SetError(upstreamStreamFailed, "The upstream stream reported an error.")
	case result.Outcome == Truncated && result.Err != nil:
		o.SetError("upstream_stream_error", result.Err.Error())
	case result.Outcome == Truncated:
		o.SetError("upstream_stream_truncated", "The upstream stream ended before completing.")
	}
	o.SetExtendedUsage(u.CachedTokens, u.ReasoningTokens, u.Cost, string(u.CostSource))
	o.RecordCompletion(u.In, u.Out)
}

// StreamMeasurement collects timings and counts without publishing inference telemetry.
// Construct it with NewStreamMeasurement so content timestamps have a clock.
type StreamMeasurement struct {
	now                                 func() time.Time
	FirstContentToken, LastContentToken time.Time
	InputTokens, OutputTokens           *int
	ErrorCode                           string
}

// NewStreamMeasurement requires the caller's clock for content timestamps.
func NewStreamMeasurement(now func() time.Time) (*StreamMeasurement, error) {
	if now == nil {
		return nil, errors.New("stream measurement requires a clock")
	}
	return &StreamMeasurement{now: now}, nil
}

// ContentToken timestamps content with the caller's clock.
func (m *StreamMeasurement) ContentToken() bool {
	m.LastContentToken = m.now()
	if m.FirstContentToken.IsZero() {
		m.FirstContentToken = m.LastContentToken
	}
	return true
}

func (*StreamMeasurement) SetResponseModel(string)    {}
func (*StreamMeasurement) CaptureText() bool          { return false }
func (*StreamMeasurement) AppendResponseText(string)  {}
func (*StreamMeasurement) MeasureTokenPresence() bool { return true }
func (*StreamMeasurement) Usage(UsageData)            {}

// End keeps missing usage distinct from reported zeroes and retains the wire failure code.
func (m *StreamMeasurement) End(result Result, u UsageData) {
	if u.HasInput {
		m.InputTokens = new(int(u.In))
	}
	if u.HasOutput {
		m.OutputTokens = new(int(u.Out))
	}
	failure, _ := result.Failure()
	m.ErrorCode = failure.Code
	if result.Outcome == UpstreamFailed {
		m.ErrorCode = upstreamStreamFailed
	}
}
