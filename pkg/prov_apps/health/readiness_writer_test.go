package health

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadinessWriter_SuccessPattern(t *testing.T) {
	var buf bytes.Buffer
	probe := &ReadinessProbe{
		LogPatterns: LogPatterns{
			Success: []PatternMatcher{{Pattern: "INFO - Starting httpd"}},
		},
		Timeout: 10 * time.Second,
	}
	require.NoError(t, probe.Validate())

	rw := NewReadinessWriter(&buf, probe)

	// Write log lines — the success pattern should match
	_, _ = rw.Write([]byte("Loading model...\n"))
	_, _ = rw.Write([]byte("INFO - Starting httpd at 127.0.0.1 on port 8090...\n"))

	select {
	case sig := <-rw.Signal:
		assert.True(t, sig.Ready)
		assert.False(t, sig.Failed)
		assert.Equal(t, "INFO - Starting httpd", sig.Pattern)
	case <-time.After(time.Second):
		t.Fatal("expected readiness signal")
	}

	// Underlying writer should have received all data
	assert.Contains(t, buf.String(), "Loading model")
	assert.Contains(t, buf.String(), "INFO - Starting httpd")
}

func TestReadinessWriter_FailurePattern(t *testing.T) {
	var buf bytes.Buffer
	probe := &ReadinessProbe{
		LogPatterns: LogPatterns{
			Success: []PatternMatcher{{Pattern: "Ready"}},
			Failure: []PatternMatcher{{Pattern: "out of memory"}},
		},
		Timeout: 10 * time.Second,
	}
	require.NoError(t, probe.Validate())

	rw := NewReadinessWriter(&buf, probe)

	_, _ = rw.Write([]byte("Loading...\n"))
	_, _ = rw.Write([]byte("FATAL: out of memory\n"))

	select {
	case sig := <-rw.Signal:
		assert.False(t, sig.Ready)
		assert.True(t, sig.Failed)
		assert.Equal(t, "out of memory", sig.Pattern)
	case <-time.After(time.Second):
		t.Fatal("expected failure signal")
	}
}

func TestReadinessWriter_FailureTakesPriority(t *testing.T) {
	var buf bytes.Buffer
	probe := &ReadinessProbe{
		LogPatterns: LogPatterns{
			Success: []PatternMatcher{{Pattern: "started"}},
			Failure: []PatternMatcher{{Pattern: "Error"}},
		},
		Timeout: 10 * time.Second,
	}
	require.NoError(t, probe.Validate())

	rw := NewReadinessWriter(&buf, probe)

	// Line contains both a success and failure pattern — failure wins
	_, _ = rw.Write([]byte("Error: server started but crashed\n"))

	select {
	case sig := <-rw.Signal:
		assert.True(t, sig.Failed)
		assert.False(t, sig.Ready)
	case <-time.After(time.Second):
		t.Fatal("expected signal")
	}
}

func TestReadinessWriter_StopsAfterFirstMatch(t *testing.T) {
	var buf bytes.Buffer
	probe := &ReadinessProbe{
		LogPatterns: LogPatterns{
			Success: []PatternMatcher{{Pattern: "Ready"}},
			Failure: []PatternMatcher{{Pattern: "Error"}},
		},
		Timeout: 10 * time.Second,
	}
	require.NoError(t, probe.Validate())

	rw := NewReadinessWriter(&buf, probe)

	_, _ = rw.Write([]byte("Ready\n"))
	<-rw.Signal // consume first signal

	// Subsequent writes should not produce more signals
	_, _ = rw.Write([]byte("Error\n"))

	select {
	case <-rw.Signal:
		t.Fatal("should not get a second signal")
	default:
		// expected — no second signal
	}

	// All data still reaches underlying writer
	assert.Contains(t, buf.String(), "Error")
}

func TestReadinessWriter_PartialLines(t *testing.T) {
	var buf bytes.Buffer
	probe := &ReadinessProbe{
		LogPatterns: LogPatterns{
			Success: []PatternMatcher{{Pattern: "httpd started"}},
		},
		Timeout: 10 * time.Second,
	}
	require.NoError(t, probe.Validate())

	rw := NewReadinessWriter(&buf, probe)

	// Write in chunks that split across the pattern
	_, _ = rw.Write([]byte("INFO - httpd "))
	_, _ = rw.Write([]byte("started on port 8090\n"))

	select {
	case sig := <-rw.Signal:
		assert.True(t, sig.Ready)
	case <-time.After(time.Second):
		t.Fatal("expected readiness signal from reassembled line")
	}
}

func TestReadinessWriter_NoMatch(t *testing.T) {
	var buf bytes.Buffer
	probe := &ReadinessProbe{
		LogPatterns: LogPatterns{
			Success: []PatternMatcher{{Pattern: "Ready"}},
		},
		Timeout: 10 * time.Second,
	}
	require.NoError(t, probe.Validate())

	rw := NewReadinessWriter(&buf, probe)

	_, _ = rw.Write([]byte("Loading model...\n"))
	_, _ = rw.Write([]byte("Initializing weights...\n"))

	select {
	case <-rw.Signal:
		t.Fatal("should not get a signal when no pattern matches")
	default:
		// expected
	}
}
