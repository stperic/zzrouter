package health

import (
	"bytes"
	"io"
	"sync"
)

// ReadinessSignal is the result of log-based readiness detection.
type ReadinessSignal struct {
	Ready   bool   // True if a success pattern matched
	Failed  bool   // True if a failure pattern matched
	Pattern string // The pattern that matched
}

// ReadinessWriter wraps an io.Writer and scans each line of process output
// against readiness probe patterns. When a success or failure pattern matches,
// a signal is sent on the Signal channel. After the first match, scanning stops
// but writing continues to the underlying writer.
//
// This allows the health monitor to detect readiness from process logs
// (e.g., "INFO - Starting httpd") instead of waiting for HTTP health checks.
type ReadinessWriter struct {
	inner    io.Writer
	probe    *ReadinessProbe
	Signal   chan ReadinessSignal
	buffer   bytes.Buffer
	mu       sync.Mutex
	resolved bool // true after first match — stop scanning
}

// NewReadinessWriter creates a writer that scans for readiness patterns.
// The probe's patterns must already be compiled (call Validate() first).
// Signal channel is buffered(1) so sending never blocks the write path.
func NewReadinessWriter(inner io.Writer, probe *ReadinessProbe) *ReadinessWriter {
	return &ReadinessWriter{
		inner:  inner,
		probe:  probe,
		Signal: make(chan ReadinessSignal, 1),
	}
}

// Write writes to the underlying writer and scans for readiness patterns.
func (rw *ReadinessWriter) Write(p []byte) (int, error) {
	// Always write to the underlying writer first
	n, err := rw.inner.Write(p)

	// Scan for patterns if not yet resolved
	rw.mu.Lock()
	if !rw.resolved {
		rw.buffer.Write(p[:n])
		rw.scanLinesLocked()
	}
	rw.mu.Unlock()

	return n, err
}

// scanLinesLocked checks complete lines against readiness patterns.
// Caller must hold rw.mu.
func (rw *ReadinessWriter) scanLinesLocked() {
	for {
		line, err := rw.buffer.ReadString('\n')
		if err != nil {
			// Incomplete line — put it back
			if line != "" {
				remaining := make([]byte, len(line)+rw.buffer.Len())
				copy(remaining, line)
				copy(remaining[len(line):], rw.buffer.Bytes())
				rw.buffer.Reset()
				rw.buffer.Write(remaining)
			}
			return
		}

		// Trim trailing newline/carriage return
		line = trimLine(line)

		// Check failure first — failure takes priority over success
		if matched, pattern := rw.probe.LogPatterns.MatchFailure(line); matched {
			rw.resolved = true
			rw.Signal <- ReadinessSignal{Failed: true, Pattern: pattern}
			return
		}

		// Check success
		if matched, pattern := rw.probe.LogPatterns.MatchSuccess(line); matched {
			rw.resolved = true
			rw.Signal <- ReadinessSignal{Ready: true, Pattern: pattern}
			return
		}
	}
}

func trimLine(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '\r' {
		s = s[:len(s)-1]
	}
	return s
}
