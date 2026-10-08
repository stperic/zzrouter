package logs

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
)

// SSEFrame is one decoded SSE frame. A frame is a run of contiguous
// non-blank lines terminated by a blank line. This reader supports the
// minimal subset used by zzrouter: "event:" and "data:" fields. Multi-
// line "data:" values are joined with newlines per the SSE spec.
//
// This type is deliberately source-agnostic. Both the run-logs session
// and a future inference-logs session can consume the same frames — the
// only thing that differs between sources is how they interpret
// Event/Data values.
type SSEFrame struct {
	Event string // e.g. "log", "done", "error"; empty if not set
	Data  string // decoded data payload; may contain newlines
}

// ReadSSE reads SSE frames from r and sends them to out until the
// context is cancelled, the reader returns io.EOF, or the reader
// returns a non-recoverable error.
//
// The function is blocking; callers typically spawn it in a goroutine.
// It always closes out before returning so consumers can use a simple
// for-range loop. If the reader returns an error other than io.EOF,
// that error is returned; otherwise nil is returned.
//
// Cancellation: ReadSSE does not itself close r — the caller is
// expected to close the underlying response body from a separate
// goroutine when ctx is cancelled, which unblocks the scanner. This
// keeps the function usable with any io.Reader and matches Go's usual
// pattern for cancellable I/O.
func ReadSSE(ctx context.Context, r io.Reader, out chan<- SSEFrame) error {
	defer close(out)

	scanner := bufio.NewScanner(r)
	// Generous buffer — provider logs occasionally emit long lines.
	// Server caps at 1 MiB, so match that.
	const maxLine = 1 << 20
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)

	var cur SSEFrame
	var dataLines []string

	flush := func() bool {
		if cur.Event == "" && len(dataLines) == 0 {
			return true
		}
		cur.Data = strings.Join(dataLines, "\n")
		select {
		case <-ctx.Done():
			return false
		case out <- cur:
		}
		cur = SSEFrame{}
		dataLines = dataLines[:0]
		return true
	}

	for scanner.Scan() {
		// Fast-path cancellation check between lines. The scanner
		// itself is unblocked by the caller closing the body.
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line := scanner.Text()

		// Blank line = end of frame.
		if line == "" {
			if !flush() {
				return nil
			}
			continue
		}

		// Comment lines (start with ':') are ignored per the SSE spec.
		// zzrouter doesn't emit these today but be defensive.
		if strings.HasPrefix(line, ":") {
			continue
		}

		name, value, ok := splitSSEField(line)
		if !ok {
			continue
		}

		switch name {
		case "event":
			cur.Event = value
		case "data":
			dataLines = append(dataLines, value)
		default:
			// "id" and "retry" are part of SSE but unused by zzrouter.
			// Ignore unknown fields rather than erroring — forward
			// compatibility.
		}
	}

	// Flush any trailing frame that wasn't terminated by a blank line.
	// This is rare in practice (servers always send the terminating
	// blank) but handles graceful EOF without data loss.
	_ = flush()

	if err := scanner.Err(); err != nil {
		// A closed body (caller-initiated shutdown) surfaces as a
		// variety of errors depending on the stack. Treat them as
		// clean shutdown.
		if errors.Is(err, io.ErrClosedPipe) || isClosedConn(err) {
			return nil
		}
		return err
	}
	return nil
}

// splitSSEField parses one SSE field line of the form "name: value"
// (the single leading space after the colon is optional per spec).
// Returns ok=false for lines that have no colon.
func splitSSEField(line string) (name, value string, ok bool) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", "", false
	}
	name = line[:idx]
	value = line[idx+1:]
	// Per SSE spec, a single leading space after the colon is stripped.
	value = strings.TrimPrefix(value, " ")
	return name, value, true
}

// isClosedConn reports whether err looks like a "use of closed
// connection" error, which is how net/http surfaces a body that was
// closed from another goroutine during Read. Prefers errors.Is against
// the standard sentinel, with a string-contains fallback for errors
// that don't wrap (e.g. http.ErrBodyReadAfterClose, transport-level
// responseBodyError strings).
func isClosedConn(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "use of closed") ||
		strings.Contains(msg, "closed network connection") ||
		strings.Contains(msg, "response body closed")
}
