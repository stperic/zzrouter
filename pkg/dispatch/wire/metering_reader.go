package wire

import (
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// meteringReader feeds a recorder from a streaming body that something
// else is copying.
//
// CopyWithMetrics is the canonical metering path, but it owns its copy
// loop, and a response served by httputil.ReverseProxy does not: the
// proxy reads the body itself. Local on-demand instances are proxied
// that way, so their streams reached the client without anything ever
// observing them and every one of them was absent from the inference
// log and the spend totals, while the non-streaming half of the same
// function recorded normally. Wrapping the body puts the observation
// back where the bytes are.
//
// Wrap the RAW body, inside any transform, for the reason CopyWithMetrics
// observes before transforming: a usage-injecting transform reads the
// live recorder, so metering has to have run first.
//
// Read and Close are serialized against each other. sync.Once alone
// would make the completion happen once but would NOT protect the usage
// it publishes: that is written by Read, and llm.InferenceRecorder is
// itself documented as unsafe for concurrent use. Close exists precisely
// for the hangup case, so "the same goroutine always does both" is a
// property this type should not have to assume about its caller.
type meteringReader struct {
	r        io.ReadCloser
	recorder *llm.InferenceRecorder
	finished sync.Once

	// mu serializes every touch of the recorder, not just of the fields
	// below: llm.InferenceRecorder is documented unsafe for concurrent
	// use, so RecordFirstToken on the read side and RecordCompletion on
	// the close side would race each other inside the recorder even if
	// this type's own state were clean. Uncontended in the common case:
	// one lock per Read, against a JSON scan of the chunk.
	mu     sync.Mutex
	first  bool
	best   UsageData
	framer lineFramer
}

// NewMeteringReader returns r wrapped so the recorder sees every chunk
// and gets its completion when the stream ends. A nil recorder returns r
// untouched, so callers need no special case.
func NewMeteringReader(r io.ReadCloser, recorder *llm.InferenceRecorder) io.ReadCloser {
	if recorder == nil {
		return r
	}
	return &meteringReader{r: r, recorder: recorder, first: true}
}

func (m *meteringReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		m.mu.Lock()
		// The bytes go to the client as they are; only the observation
		// is line-framed, because the parse behind it is per-line and a
		// usage frame delivered in two reads would otherwise meter as
		// zero. TTFT stays on the raw read, which is when the client
		// actually got something.
		if frame := m.framer.next(p[:n]); len(frame) > 0 {
			m.observeLocked(frame)
		}
		if m.first {
			m.recorder.RecordFirstToken()
			m.first = false
		}
		m.mu.Unlock()
	}
	if err != nil {
		if !errors.Is(err, io.EOF) {
			// Same reasoning as CopyWithMetrics: a stream that breaks
			// mid-flight must not emit as though it succeeded.
			slog.Error("Stream read error", "error", err)
			m.mu.Lock()
			m.recorder.SetError("upstream_stream_error", err.Error())
			m.mu.Unlock()
		}
		m.finish()
	}
	return n, err
}

// Close covers the stream that ends without ever returning EOF — a
// client that disconnects leaves the proxy closing the body mid-read,
// and the tokens spent up to that point still have to be attributed.
func (m *meteringReader) Close() error {
	m.finish()
	return m.r.Close()
}

// observeLocked feeds one whole-line frame to the recorder, merging its
// usage into the running total. Callers hold mu.
func (m *meteringReader) observeLocked(frame []byte) {
	if u, ok := observeChunk(m.recorder, frame); ok {
		m.best = m.best.merge(u)
	}
}

func (m *meteringReader) finish() {
	m.finished.Do(func() {
		// Held across the recorder calls, not just the field read.
		m.mu.Lock()
		defer m.mu.Unlock()

		// The stream is over, so a final frame that never got its
		// newline is complete: observe it before publishing, or an
		// upstream that omits the trailing newline meters as zero.
		if tail := m.framer.flush(); len(tail) > 0 {
			m.observeLocked(tail)
		}

		m.recorder.SetExtendedUsage(m.best.CachedTokens, m.best.ReasoningTokens, m.best.Cost, string(m.best.CostSource))
		m.recorder.RecordCompletion(m.best.In, m.best.Out)
	})
}
