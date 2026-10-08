package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SSEFrame is one decoded Server-Sent Events frame. The harness's SSE
// parser is intentionally minimal — no auto-reconnect, no Last-Event-ID
// retry — because the only consumers are tests, and a flaky stream
// should fail loud, not silently retry.
type SSEFrame struct {
	Event string
	ID    string
	Data  string
	Raw   []byte // exact bytes of the frame, for diagnostic dumps
}

// TailSSE drives an open SSE response to completion. onFrame is called
// once per frame; returning true stops the consumer cleanly. The
// caller is responsible for setting the Accept header before opening
// the response. Closes resp.Body before returning.
//
// ctx cancellation closes the body (forcing the blocked Read to
// return), which is the only portable way to interrupt bufio reads
// across all transports.
func TailSSE(ctx context.Context, resp *http.Response, onFrame func(SSEFrame) bool) error {
	if resp == nil || resp.Body == nil {
		return errors.New("nil response or body")
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "text/event-stream") {
		return fmt.Errorf("not an SSE stream (Content-Type=%q)", ct)
	}

	// Close-on-cancel watcher. context.AfterFunc returns a stop func
	// that we call on every return path so the goroutine is reaped
	// promptly when the stream ends naturally.
	stop := context.AfterFunc(ctx, func() { _ = resp.Body.Close() })
	defer stop()

	br := bufio.NewReader(resp.Body)
	var (
		event   string
		id      string
		dataBuf bytes.Buffer
		raw     bytes.Buffer
	)
	flush := func() bool {
		if event == "" && id == "" && dataBuf.Len() == 0 {
			return false // empty separator
		}
		f := SSEFrame{
			Event: event,
			ID:    id,
			Data:  strings.TrimSuffix(dataBuf.String(), "\n"),
			Raw:   append([]byte(nil), raw.Bytes()...),
		}
		event = ""
		id = ""
		dataBuf.Reset()
		raw.Reset()
		return onFrame(f)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) {
			// Some servers close without a trailing blank line; flush whatever's buffered.
			if dataBuf.Len() > 0 || event != "" || id != "" {
				flush()
			}
			return nil
		}
		if err != nil {
			// If ctx was cancelled, surface the cancellation rather
			// than the synthetic "use of closed network connection"
			// error AfterFunc induces.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("sse read: %w", err)
		}
		raw.WriteString(line)
		line = strings.TrimRight(line, "\r\n")

		// Blank line = frame boundary.
		if line == "" {
			if flush() {
				return nil
			}
			continue
		}
		// Comments per spec: line starts with ':' — skip.
		if strings.HasPrefix(line, ":") {
			continue
		}
		// Field:value pairs.
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field = line
			value = ""
		}
		// Spec: a single optional space after the colon is stripped.
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "event":
			event = value
		case "id":
			id = value
		case "data":
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(value)
		case "retry":
			// Retry hints are advisory; the harness ignores them.
		}
	}
}
