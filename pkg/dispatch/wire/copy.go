package wire

import (
	"context"
	"io"
	"net/http"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// CopyStream relays a stream with immediate flushing and reports how it ended. It never writes a
// terminal frame of its own: ending a broken stream is the caller's, per dialect.
func CopyStream(ctx context.Context, w http.ResponseWriter, r io.Reader, recorder *llm.InferenceRecorder, transform func([]byte) []byte) Result {
	flusher, canFlush := w.(http.Flusher)
	meter := streamMeter{recorder: recorder}
	emit := func(frame []byte) error {
		if len(frame) == 0 {
			return nil
		}
		// Meter raw bytes first because transforms read the live recorder.
		if transform != nil {
			frame = transform(frame)
		}
		n, err := w.Write(frame)
		if err == nil && n != len(frame) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return err
		}
		if canFlush {
			flusher.Flush()
		}
		return nil
	}

	buf := make([]byte, StreamBufferSize)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if err := emit(meter.next(buf[:n])); err != nil {
				return meter.finish(err, true)
			}
		}
		if readErr != nil {
			if err := emit(meter.tail()); err != nil {
				return meter.finish(err, true)
			}
			return meter.finish(readErr, ctx.Err() != nil)
		}
	}
}
