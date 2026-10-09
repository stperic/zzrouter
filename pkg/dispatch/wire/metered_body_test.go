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

func TestMeteredBodyPassesBytesThrough(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}

	got, err := io.ReadAll(NewMeteredBody(context.Background(), body, rec))
	require.NoError(t, err)

	assert.Equal(t, usageStream, string(got), "metering must not alter the stream")
}

func TestMeteredBodyObservesUsage(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}
	time.Sleep(2 * time.Millisecond)

	_, err := io.ReadAll(NewMeteredBody(context.Background(), body, rec))
	require.NoError(t, err)

	snap := rec.Snapshot()
	assert.Equal(t, int64(7), snap.TokensOut, "terminal usage frame must reach the recorder")
	assert.Positive(t, snap.TTFTMs, "first chunk must record time-to-first-token")
}

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

func installCompletionSpy(t *testing.T) *completionSpy {
	t.Helper()
	spy := &completionSpy{}
	llm.SetInferenceLogHook(spy)
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
	return spy
}

func TestMeteredBodyPublishesCompletionOnEOF(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}

	_, err := io.ReadAll(NewMeteredBody(context.Background(), body, rec))
	require.NoError(t, err)

	calls := spy.snapshot()
	require.Len(t, calls, 1, "exactly one completion per stream")
	assert.Equal(t, int64(11), calls[0].TokensIn)
	assert.Equal(t, int64(7), calls[0].TokensOut)
}

func TestMeteredBodyPublishesCompletionOnClose(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}
	mr := NewMeteredBody(context.Background(), body, rec)

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
func TestMeteredBodyPublishesCompletionOnce(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(usageStream)}
	mr := NewMeteredBody(context.Background(), body, rec)

	_, err := io.ReadAll(mr)
	require.NoError(t, err)
	require.NoError(t, mr.Close())

	assert.Len(t, spy.snapshot(), 1, "EOF and Close must collapse to one completion")
}

func TestMeteredBodyNilRecorderIsPassThrough(t *testing.T) {
	body := &countingCloser{Reader: strings.NewReader(usageStream)}

	mr := NewMeteredBody(context.Background(), body, nil)
	got, err := io.ReadAll(mr)
	require.NoError(t, err)
	require.NoError(t, mr.Close())
	assert.Equal(t, usageStream, string(got))
	assert.Equal(t, Completed, mr.Result().Outcome)
}

func TestMeteredBodyConcurrentReadAndClose(t *testing.T) {
	installCompletionSpy(t)

	for range 50 {
		rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
		mr := NewMeteredBody(context.Background(), &countingCloser{Reader: strings.NewReader(usageStream)}, rec)

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

func TestMeteredBodyMetersUsageFrameSplitAcrossReads(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")

	cut := strings.Index(usageStream, `"completion_tokens"`) + 8
	body := &pieceReader{pieces: []string{usageStream[:cut], usageStream[cut:]}}

	got, err := io.ReadAll(NewMeteredBody(context.Background(), body, rec))
	require.NoError(t, err)

	calls := spy.snapshot()
	require.Len(t, calls, 1, "exactly one completion per stream")
	assert.Equal(t, int64(11), calls[0].TokensIn, "a split usage frame must still meter its input tokens")
	assert.Equal(t, int64(7), calls[0].TokensOut, "a split usage frame must still meter its output tokens")
	assert.Equal(t, usageStream, string(got), "observation must not alter the stream")
}

func TestMeteredBodyMetersFinalFrameWithoutTrailingNewline(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(
		`data: {"model":"m","usage":{"prompt_tokens":4,"completion_tokens":2}}`)}

	_, err := io.ReadAll(NewMeteredBody(context.Background(), body, rec))
	require.NoError(t, err)

	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut, "the held tail must be observed before the completion")
}

func TestMeteredBodyMetersHeldTailOnClose(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := &countingCloser{Reader: strings.NewReader(
		`data: {"model":"m","usage":{"prompt_tokens":4,"completion_tokens":2}}`)}

	mr := NewMeteredBody(context.Background(), body, rec)
	buf := make([]byte, 512)
	_, err := mr.Read(buf) // reads the whole frame, newline never arrives
	require.NoError(t, err)
	require.NoError(t, mr.Close())

	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut, "close must flush the framer before publishing")
}
