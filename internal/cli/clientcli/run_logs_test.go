package clientcli

// Tests for the non-TTY CLI surface of `zzrouter run logs`: the runs
// table formatter, the streaming core, and the runRunLogs dispatcher's
// no-match path. These tests deliberately bypass the Cobra layer and
// signal handling — streamRunLogsToStdout's signal goroutine is tested
// by running streamRunLogsTo (the writer-parameterized core) with a
// cancellable context.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	logsclient "github.com/stperic/zzrouter/internal/client/logs"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// newRunLogsTestClient spins up an httptest server driven by handler
// and returns a client targeted at it. The server is closed via
// t.Cleanup.
func newRunLogsTestClient(t *testing.T, handler http.HandlerFunc) *pkgClient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return pkgClient.NewClient(pkgConfig.ClientNodeConfig{
		Name:    "test",
		Address: srv.URL,
		APIKey:  "test-key",
	})
}

// runLogsTestServer is a minimal mirror of sessionTestServer from the
// logs package, duplicated here to keep the two packages independent.
type runLogsTestServer struct {
	runs []pkgClient.Instance

	mu         sync.Mutex
	tailLines  []string
	totalLines int
	tailFails  bool

	streamFrames [][]struct{ event, data string }
	streamIdx    int
}

func (s *runLogsTestServer) setTail(lines []string, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tailLines = append([]string(nil), lines...)
	s.totalLines = total
}

func (s *runLogsTestServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zzrouter/v1/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": s.runs})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			if r.URL.Query().Get("follow") == "true" {
				s.serveSSE(w, r)
				return
			}
			s.mu.Lock()
			fails := s.tailFails
			lines := append([]string(nil), s.tailLines...)
			total := s.totalLines
			s.mu.Unlock()
			if fails {
				http.Error(w, "tail failure", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"lines":       lines,
					"total_lines": total,
					"count":       len(lines),
				},
			})
		default:
			http.NotFound(w, r)
		}
	}
}

func (s *runLogsTestServer) serveSSE(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var frames []struct{ event, data string }
	if s.streamIdx < len(s.streamFrames) {
		frames = s.streamFrames[s.streamIdx]
		s.streamIdx++
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for _, f := range frames {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, f.data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// After scripted frames, hold the connection open so the test can
	// exercise the context-cancel path.
	<-r.Context().Done()
}

// TestWriteRunsTable_OutputShape verifies the tab-separated header and
// row shape. The CLI contract is that downstream tools (awk, cut) can
// rely on stable column order: RUN ID, PROVIDER, MODEL, NODE, STATUS,
// STARTED.
func TestWriteRunsTable_OutputShape(t *testing.T) {
	runs := []logsclient.Run{
		{ID: "r_1", Provider: "mlx", Model: "llama-3", Node: "coord", Status: "running", StartedAt: "2026-04-11T10:00:00Z"},
		{ID: "r_2", Provider: "vllm", Model: "qwen", Node: "worker", Status: "stopped", StartedAt: "2026-04-11T09:00:00Z"},
	}
	var buf bytes.Buffer
	if err := writeRunsTable(&buf, runs); err != nil {
		t.Fatalf("writeRunsTable: %v", err)
	}
	out := buf.String()

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (header + 2 rows), got %d:\n%s", len(lines), out)
	}
	header := lines[0]
	for _, col := range []string{"RUN ID", "PROVIDER", "MODEL", "NODE", "STATUS", "STARTED"} {
		if !strings.Contains(header, col) {
			t.Errorf("header missing column %q: %q", col, header)
		}
	}
	// Rows must contain each field's literal value in stable order.
	if !strings.Contains(lines[1], "r_1") || !strings.Contains(lines[1], "mlx") || !strings.Contains(lines[1], "llama-3") {
		t.Errorf("row 1 missing fields: %q", lines[1])
	}
	if !strings.Contains(lines[2], "r_2") || !strings.Contains(lines[2], "vllm") || !strings.Contains(lines[2], "qwen") {
		t.Errorf("row 2 missing fields: %q", lines[2])
	}
	// Column order: RUN ID appears before PROVIDER appears before MODEL.
	runIDAt := strings.Index(header, "RUN ID")
	provAt := strings.Index(header, "PROVIDER")
	modelAt := strings.Index(header, "MODEL")
	if runIDAt >= provAt || provAt >= modelAt {
		t.Errorf("columns out of order in header: %q", header)
	}
}

func TestStreamRunLogsTo_StaticTailHappyPath(t *testing.T) {
	srv := &runLogsTestServer{
		runs: []pkgClient.Instance{{ID: "r_1", App: "mlx", Status: "stopped"}},
	}
	srv.setTail([]string{"hello", "world", "done"}, 3)
	c := newRunLogsTestClient(t, srv.handler())

	run := logsclient.Run{ID: "r_1", Provider: "mlx", Status: "stopped"}
	var stdout, stderr bytes.Buffer
	err := streamRunLogsTo(context.Background(), c, run, 100, false, &stdout, &stderr)
	if err != nil {
		t.Fatalf("streamRunLogsTo: %v", err)
	}
	got := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	want := []string{"hello", "world", "done"}
	if len(got) != len(want) {
		t.Fatalf("stdout lines: want %d got %d (%q)", len(want), len(got), stdout.String())
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("line %d: want %q got %q", i, w, got[i])
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}

func TestStreamRunLogsTo_TailFetchErrorReturnsError(t *testing.T) {
	srv := &runLogsTestServer{
		runs: []pkgClient.Instance{{ID: "r_1", App: "mlx", Status: "stopped"}},
	}
	srv.tailFails = true
	c := newRunLogsTestClient(t, srv.handler())

	run := logsclient.Run{ID: "r_1", Provider: "mlx", Status: "stopped"}
	var stdout, stderr bytes.Buffer
	err := streamRunLogsTo(context.Background(), c, run, 100, false, &stdout, &stderr)
	if err == nil {
		t.Fatal("want error from failing tail fetch, got nil")
	}
	if !strings.Contains(err.Error(), "fetch logs") {
		t.Errorf("error should be wrapped with 'fetch logs:': %v", err)
	}
}

// TestStreamRunLogsTo_ContextCancelShutsDownCleanly exercises the
// running-run path: the function opens a RunSession, cancelling ctx
// must unblock the events loop and return promptly. This is the code
// path SIGINT takes in streamRunLogsToStdout (its signal goroutine
// cancels the ctx passed to streamRunLogsTo).
func TestStreamRunLogsTo_ContextCancelShutsDownCleanly(t *testing.T) {
	srv := &runLogsTestServer{
		runs: []pkgClient.Instance{{ID: "r_1", App: "mlx", Status: "running"}},
	}
	srv.setTail([]string{"a"}, 1)
	c := newRunLogsTestClient(t, srv.handler())

	run := logsclient.Run{ID: "r_1", Provider: "mlx", Status: "running"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		done <- streamRunLogsTo(ctx, c, run, 100, false, &stdout, &stderr)
	}()

	// Give the tail a moment to reach stdout, then cancel.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// Cancellation is not itself an error; the function returns
		// nil after draining the channel.
		if err != nil {
			t.Errorf("want nil after ctx cancel, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamRunLogsTo did not return within 2s of ctx cancel")
	}

	// The tail entry should have landed in stdout before cancellation.
	if !strings.Contains(stdout.String(), "a") {
		t.Errorf("stdout should contain tail entry 'a', got %q", stdout.String())
	}
}

// TestRunRunLogs_NoMatchReturnsError drives the top-level dispatcher
// with a filter that matches nothing; it should surface ErrNoMatch so
// Cobra maps the return to a non-zero exit code (via SilenceErrors).
func TestRunRunLogs_NoMatchReturnsError(t *testing.T) {
	srv := &runLogsTestServer{runs: nil}
	c := newRunLogsTestClient(t, srv.handler())

	err := runRunLogs(context.Background(), c, logsclient.RunFilter{Provider: "nosuch"}, 100, false, false)
	if err == nil {
		t.Fatal("want error for no-match, got nil")
	}
	if !errors.Is(err, logsclient.ErrNoMatch) {
		t.Errorf("want ErrNoMatch, got %v", err)
	}
}
