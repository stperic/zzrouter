package health

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ReadinessSignal is the result of log-based readiness detection.
type ReadinessSignal struct {
	Cause   *httperr.EngineFailure
	Ready   bool   // True if a success pattern matched
	Failed  bool   // True if a failure pattern matched
	Pattern string // The pattern that matched
}

// readinessSignalCapacity holds startup and failure signals from one write.
const readinessSignalCapacity = 2

// ReadinessWriter keeps checking failures after startup when a serve probe is present.
type ReadinessWriter struct {
	origin       time.Time
	lastOutputAt atomic.Int64
	lastLine     []byte
	lineEnded    bool
	inner        io.Writer
	probe        *ReadinessProbe
	Signal       chan ReadinessSignal
	buffer       bytes.Buffer
	mu           sync.Mutex
	started      bool
	resolved     bool // true after first match — stop scanning, capture included
	capture      *regexp.Regexp
	onCapture    func([]string)
}

// NewReadinessWriter creates a writer that scans for readiness patterns.
// The probe's patterns must already be compiled (call Validate() first).
// Signals never block the write path.
// The optional capture callback fires once, outside the writer lock, for a
// line seen before readiness resolves (engines log placement before ready).
func NewReadinessWriter(inner io.Writer, probe *ReadinessProbe, capture *regexp.Regexp, onCapture func([]string)) *ReadinessWriter {
	return &ReadinessWriter{
		origin:  utils.Now(),
		inner:   inner,
		probe:   probe,
		Signal:  make(chan ReadinessSignal, readinessSignalCapacity),
		capture: capture, onCapture: onCapture,
	}
}

// Write writes to the underlying writer and scans for readiness patterns.
func (rw *ReadinessWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		rw.lastOutputAt.Store(int64(utils.Now().Sub(rw.origin)))
	}
	n, err := rw.inner.Write(p)

	var match []string
	rw.mu.Lock()
	rw.recordOutputLocked(p[:n])
	if !rw.resolved {
		rw.buffer.Write(p[:n])
		match = rw.scanLinesLocked()
	}
	rw.mu.Unlock()
	if match != nil && rw.onCapture != nil {
		rw.onCapture(match[1:])
	}

	return n, err
}

// LastOutputAt preserves monotonic time by storing elapsed nanoseconds atomically.
func (rw *ReadinessWriter) LastOutputAt() time.Time {
	return rw.origin.Add(time.Duration(rw.lastOutputAt.Load()))
}

const maxReadinessLineBytes = 300

// LastLine returns bounded output, including an unfinished line.
func (rw *ReadinessWriter) LastLine() string {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return strings.ToValidUTF8(string(rw.lastLine), "")
}

func (rw *ReadinessWriter) recordOutputLocked(p []byte) {
	for _, b := range p {
		if b == '\n' || b == '\r' {
			rw.lineEnded = true
			continue
		}
		if rw.lineEnded {
			rw.lastLine = rw.lastLine[:0]
			rw.lineEnded = false
		}
		if len(rw.lastLine) < maxReadinessLineBytes {
			rw.lastLine = append(rw.lastLine, b)
		}
	}
}

// scanLinesLocked checks complete lines against readiness patterns.
// Caller must hold rw.mu.
func (rw *ReadinessWriter) scanLinesLocked() (match []string) {
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
		if rw.capture != nil {
			if match = rw.capture.FindStringSubmatch(line); match != nil {
				rw.capture = nil
			}
		}
		if cause := rw.probe.FailureClassifier.Match(line); cause != nil {
			rw.resolved = true
			rw.Signal <- ReadinessSignal{Failed: true, Pattern: line, Cause: cause}
			return
		}

		// Check failure first — failure takes priority over success
		if matched, pattern := rw.probe.LogPatterns.MatchFailure(line); matched {
			rw.resolved = true
			rw.Signal <- ReadinessSignal{Failed: true, Pattern: pattern}
			return
		}

		// Check success
		if matched, pattern := rw.probe.LogPatterns.MatchSuccess(line); matched && !rw.started {
			rw.resolved = rw.probe.Serve == nil
			rw.started = true
			rw.Signal <- ReadinessSignal{Ready: true, Pattern: pattern}
			if rw.resolved {
				return
			}
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
