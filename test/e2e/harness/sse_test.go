package harness

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer responds with the supplied body verbatim under text/event-stream.
func sseServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
}

func TestTailSSE_BasicFrames(t *testing.T) {
	body := "event: progress\ndata: {\"phase\":\"running\",\"seq\":1}\n\nevent: done\ndata: {\"phase\":\"done\",\"seq\":2}\n\n"
	srv := sseServer(body)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var got []SSEFrame
	err = TailSSE(context.Background(), resp, func(f SSEFrame) bool {
		got = append(got, f)
		return f.Event == "done"
	})
	if err != nil {
		t.Fatalf("TailSSE: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(got))
	}
	if got[0].Event != "progress" || !strings.Contains(got[0].Data, "running") {
		t.Errorf("frame 0: %+v", got[0])
	}
	if got[1].Event != "done" {
		t.Errorf("frame 1: %+v", got[1])
	}
}

func TestTailSSE_RejectsNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := TailSSE(context.Background(), resp, func(SSEFrame) bool { return false }); err == nil {
		t.Fatal("expected error on non-SSE content type")
	}
}

func TestTailSSE_MultilineData(t *testing.T) {
	body := "event: msg\ndata: line1\ndata: line2\n\n"
	srv := sseServer(body)
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	var f SSEFrame
	_ = TailSSE(context.Background(), resp, func(x SSEFrame) bool {
		f = x
		return true
	})
	if f.Data != "line1\nline2" {
		t.Errorf("multiline data: got %q", f.Data)
	}
}

func TestTailSSE_IgnoresComments(t *testing.T) {
	body := ": keepalive\nevent: ping\ndata: ok\n\n"
	srv := sseServer(body)
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	count := 0
	_ = TailSSE(context.Background(), resp, func(SSEFrame) bool {
		count++
		return true
	})
	if count != 1 {
		t.Errorf("expected 1 frame ignoring comment, got %d", count)
	}
}

func TestTailSSE_ContextCancel(t *testing.T) {
	// Server holds the connection open without sending a terminal frame.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: progress\ndata: {}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		// Block until the test cancels.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	err = TailSSE(ctx, resp, func(SSEFrame) bool { return false })
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// satisfy unused-package linter when we don't need io directly
var _ = io.EOF
