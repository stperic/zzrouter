package health

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadinessWriterCaptureOnceOutsideLock(t *testing.T) {
	partial, err := os.ReadFile("../testdata/offload/partial.txt")
	require.NoError(t, err)
	full, err := os.ReadFile("../testdata/offload/full.txt")
	require.NoError(t, err)
	line := strings.SplitAfter(string(partial), "\n")[0]
	for _, scenario := range []string{"one write", "separate writes", "after ready with serve", "resolved without serve", "concurrent writes"} {
		t.Run(scenario, func(t *testing.T) {
			probe := &ReadinessProbe{Timeout: time.Second, LogPatterns: LogPatterns{Success: []PatternMatcher{{Pattern: "listening on http"}}}}
			if scenario == "after ready with serve" {
				probe.Serve = &ServeCheck{Path: "/check"}
			}
			require.NoError(t, probe.Validate())
			var captures [][]string
			var writer *ReadinessWriter
			writer = NewReadinessWriter(io.Discard, probe, regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`), func(values []string) {
				writer.LastLine()
				captures = append(captures, values)
			})
			done := make(chan struct{})
			go func() {
				defer close(done)
				if scenario == "after ready with serve" || scenario == "resolved without serve" {
					ready := strings.SplitAfter(string(partial), "\n")[1]
					_, err := writer.Write([]byte(ready))
					assert.NoError(t, err)
				}
				switch scenario {
				case "one write":
					_, err := writer.Write([]byte(line + string(full)))
					assert.NoError(t, err)
				case "concurrent writes":
					var writes sync.WaitGroup
					for range 8 {
						writes.Go(func() { _, err := writer.Write([]byte(line)); assert.NoError(t, err) })
					}
					writes.Wait()
				default:
					_, err := writer.Write([]byte(line))
					assert.NoError(t, err)
					_, err = writer.Write(full)
					assert.NoError(t, err)
				}
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("capture callback cannot reenter writer outside its lock")
			}
			want := [][]string{{"8", "29"}}
			if scenario == "resolved without serve" {
				want = nil // capture ends with readiness; engines log placement before ready
			}
			assert.Equal(t, want, captures)
		})
	}
}

func TestReadinessWriter_SuccessPattern(t *testing.T) {
	var buf bytes.Buffer
	probe := &ReadinessProbe{
		LogPatterns: LogPatterns{
			Success: []PatternMatcher{{Pattern: "INFO - Starting httpd"}},
		},
		Timeout: 10 * time.Second,
	}
	require.NoError(t, probe.Validate())

	rw := NewReadinessWriter(&buf, probe, nil, nil)

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

	rw := NewReadinessWriter(&buf, probe, nil, nil)

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

	rw := NewReadinessWriter(&buf, probe, nil, nil)

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

	rw := NewReadinessWriter(&buf, probe, nil, nil)

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

	rw := NewReadinessWriter(&buf, probe, nil, nil)

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

	rw := NewReadinessWriter(&buf, probe, nil, nil)

	_, _ = rw.Write([]byte("Loading model...\n"))
	_, _ = rw.Write([]byte("Initializing weights...\n"))

	select {
	case <-rw.Signal:
		t.Fatal("should not get a signal when no pattern matches")
	default:
		// expected
	}
}

func TestReadinessWriterFailureAfterListener(t *testing.T) {
	classifier := httperr.Classifier{{Pattern: "device allocation failed", Class: httperr.FailureMemory}}
	require.NoError(t, classifier.Compile())
	probe := &ReadinessProbe{FailureClassifier: &classifier, Serve: &ServeCheck{Path: "/check"}, LogPatterns: LogPatterns{Success: []PatternMatcher{{Pattern: "listening"}}, Failure: []PatternMatcher{{Pattern: "RuntimeError:"}}}}
	writer := NewReadinessWriter(io.Discard, probe, nil, nil)
	_, err := writer.Write([]byte("listening\nRuntimeError: device allocation failed\n"))
	require.NoError(t, err)
	assert.True(t, (<-writer.Signal).Ready)
	select {
	case signal := <-writer.Signal:
		require.NotNil(t, signal.Cause)
		assert.Equal(t, httperr.FailureMemory, signal.Cause.Class)
	case <-time.After(time.Second):
		t.Fatal("failure after listener was not classified")
	}
}
