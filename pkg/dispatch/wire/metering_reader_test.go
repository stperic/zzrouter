package wire

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// countingCloser reports how the wrapper treated the body underneath it.
type countingCloser struct {
	io.Reader
	closes int
}

func (c *countingCloser) Close() error { c.closes++; return nil }

const usageStream = "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n" +
	"data: [DONE]\n\n"

// The wrapper sits in the client's byte path, so the first thing it must
// not do is change what the client receives.
func TestMeteringReaderPassesBytesThrough(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}

	got, err := io.ReadAll(NewMeteringReader(body, rec))
	require.NoError(t, err)

	assert.Equal(t, usageStream, string(got), "metering must not alter the stream")
}

// The point of the wrapper: a stream that nothing else observes still
// lands its usage on the recorder.
func TestMeteringReaderObservesUsage(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}
	// TTFT is reported in whole milliseconds and this stream is in
	// memory, so without a floor the assertion below reads 0 for a
	// recorder that worked perfectly.
	time.Sleep(2 * time.Millisecond)

	_, err := io.ReadAll(NewMeteringReader(body, rec))
	require.NoError(t, err)

	snap := rec.Snapshot()
	assert.Equal(t, int64(7), snap.TokensOut, "terminal usage frame must reach the recorder")
	assert.Positive(t, snap.TTFTMs, "first chunk must record time-to-first-token")
}

// completionSpy captures the completion RecordCompletion publishes.
// finish() is the whole reason this type exists, and the fields it
// touches — tokensOut, TTFT — are already written on the Read side, so
// a snapshot assertion passes even with finish() gutted. Only the
// completion event distinguishes them.
type completionSpy struct {
	mu    sync.Mutex
	calls []llm.InferenceLogData
}

func (c *completionSpy) OnInferenceComplete(d llm.InferenceLogData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, d)
}

func (c *completionSpy) snapshot() []llm.InferenceLogData {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]llm.InferenceLogData(nil), c.calls...)
}

// installCompletionSpy registers a hook for the duration of one test.
// The hook is global, so tests using it must not run in parallel.
func installCompletionSpy(t *testing.T) *completionSpy {
	t.Helper()
	spy := &completionSpy{}
	llm.SetInferenceLogHook(spy)
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
	return spy
}

// Reading to EOF has to publish the completion, carrying the usage the
// stream actually reported.
func TestMeteringReaderPublishesCompletionOnEOF(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}

	_, err := io.ReadAll(NewMeteringReader(body, rec))
	require.NoError(t, err)

	calls := spy.snapshot()
	require.Len(t, calls, 1, "exactly one completion per stream")
	assert.Equal(t, int64(11), calls[0].TokensIn)
	assert.Equal(t, int64(7), calls[0].TokensOut)
}

// A caller that hangs up mid-stream is the case Read-to-EOF never
// covers: the proxy closes the body instead, and the tokens delivered up
// to that point were still spent.
func TestMeteringReaderPublishesCompletionOnClose(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}
	mr := NewMeteringReader(body, rec)

	// Read PAST the usage frame but stop before EOF, so the only thing
	// that can publish what was observed is Close.
	buf := make([]byte, len(usageStream)-len("data: [DONE]\n\n"))
	_, err := io.ReadFull(mr, buf)
	require.NoError(t, err)
	require.Empty(t, spy.snapshot(), "nothing is published until the stream ends")

	require.NoError(t, mr.Close())

	calls := spy.snapshot()
	require.Len(t, calls, 1, "a hangup still has to attribute what was spent")
	assert.Equal(t, int64(7), calls[0].TokensOut)
	assert.Equal(t, 1, body.closes, "underlying body must be closed exactly once")
}

// EOF then Close must not bill the request twice.
func TestMeteringReaderPublishesCompletionOnce(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}
	mr := NewMeteringReader(body, rec)

	_, err := io.ReadAll(mr)
	require.NoError(t, err)
	require.NoError(t, mr.Close())

	assert.Len(t, spy.snapshot(), 1, "EOF and Close must collapse to one completion")
}

// Callers pass whatever recorder they have; a nil one must not force
// them into a special case, and must hand back the very same reader
// rather than a wrapper that does nothing.
func TestMeteringReaderNilRecorderIsPassThrough(t *testing.T) {
	body := &countingCloser{Reader: strings.NewReader(usageStream)}

	assert.Same(t, io.ReadCloser(body), NewMeteringReader(body, nil))
}

// Close exists for the hangup case, which is inherently concurrent with
// the copy loop. Under -race this fails outright if the completion reads
// usage while a Read is writing it.
func TestMeteringReaderConcurrentReadAndClose(t *testing.T) {
	installCompletionSpy(t)

	for range 50 {
		rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
		mr := NewMeteringReader(&countingCloser{Reader: strings.NewReader(usageStream)}, rec)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			buf := make([]byte, 16)
			for {
				if _, err := mr.Read(buf); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			_ = mr.Close()
		}()
		wg.Wait()
	}
}

// The wrapper observes what the proxy forwards, and the proxy's reads
// land wherever the segments fall. A usage frame cut in two used to
// leave the stream metered at zero while the bytes reached the client
// intact — no error, no log line, a free request.
func TestMeteringReaderMetersUsageFrameSplitAcrossReads(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")

	cut := strings.Index(usageStream, `"completion_tokens"`) + 8
	body := &pieceReader{pieces: []string{usageStream[:cut], usageStream[cut:]}}

	got, err := io.ReadAll(NewMeteringReader(body, rec))
	require.NoError(t, err)

	calls := spy.snapshot()
	require.Len(t, calls, 1, "exactly one completion per stream")
	assert.Equal(t, int64(11), calls[0].TokensIn, "a split usage frame must still meter its input tokens")
	assert.Equal(t, int64(7), calls[0].TokensOut, "a split usage frame must still meter its output tokens")
	assert.Equal(t, usageStream, string(got), "observation must not alter the stream")
}

// An upstream whose last frame carries no trailing newline is complete
// once the stream ends, so the completion must account for it.
func TestMeteringReaderMetersFinalFrameWithoutTrailingNewline(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(
		`data: {"model":"m","usage":{"prompt_tokens":4,"completion_tokens":2}}`)}

	_, err := io.ReadAll(NewMeteringReader(body, rec))
	require.NoError(t, err)

	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut, "the held tail must be observed before the completion")
}

// The hangup path shares finish() with EOF, so the tail it holds has to
// be observed there too.
func TestMeteringReaderMetersHeldTailOnClose(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(
		`data: {"model":"m","usage":{"prompt_tokens":4,"completion_tokens":2}}`)}

	mr := NewMeteringReader(body, rec)
	buf := make([]byte, 512)
	_, err := mr.Read(buf) // reads the whole frame, newline never arrives
	require.NoError(t, err)
	require.NoError(t, mr.Close())

	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut, "close must flush the framer before publishing")
}
