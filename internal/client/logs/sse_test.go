package logs

import (
	"context"
	"io"
	"strings"
	"testing"
)

// collectFrames runs ReadSSE against a string payload and returns all
// frames it emits. Suitable only for finite/static input.
func collectFrames(t *testing.T, payload string) []SSEFrame {
	t.Helper()
	frames := make(chan SSEFrame, 16)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ReadSSE(context.Background(), strings.NewReader(payload), frames)
	}()
	var got []SSEFrame
	for f := range frames {
		got = append(got, f)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	return got
}

func TestReadSSE_SingleFrame(t *testing.T) {
	payload := "event: log\ndata: hello world\n\n"
	got := collectFrames(t, payload)
	if len(got) != 1 {
		t.Fatalf("want 1 frame, got %d", len(got))
	}
	if got[0].Event != "log" || got[0].Data != "hello world" {
		t.Fatalf("got %+v", got[0])
	}
}

func TestReadSSE_MultipleFrames(t *testing.T) {
	payload := "event: log\ndata: line 1\n\nevent: log\ndata: line 2\n\nevent: done\ndata: bye\n\n"
	got := collectFrames(t, payload)
	if len(got) != 3 {
		t.Fatalf("want 3 frames, got %d", len(got))
	}
	if got[0].Data != "line 1" || got[1].Data != "line 2" || got[2].Event != "done" {
		t.Fatalf("got %+v", got)
	}
}

func TestReadSSE_MultilineData(t *testing.T) {
	// Per SSE spec, multiple data: fields in one frame join with \n.
	payload := "event: log\ndata: first line\ndata: second line\n\n"
	got := collectFrames(t, payload)
	if len(got) != 1 {
		t.Fatalf("want 1 frame, got %d", len(got))
	}
	if got[0].Data != "first line\nsecond line" {
		t.Fatalf("got %q", got[0].Data)
	}
}

func TestReadSSE_IgnoresComments(t *testing.T) {
	payload := ": this is a comment\nevent: log\ndata: real line\n\n"
	got := collectFrames(t, payload)
	if len(got) != 1 {
		t.Fatalf("want 1 frame, got %d", len(got))
	}
	if got[0].Data != "real line" {
		t.Fatalf("got %q", got[0].Data)
	}
}

func TestReadSSE_LeadingSpaceAfterColonStripped(t *testing.T) {
	// "data:hello" and "data: hello" should both decode to "hello".
	payload := "event:log\ndata:no-space\n\nevent: log\ndata: with-space\n\n"
	got := collectFrames(t, payload)
	if len(got) != 2 {
		t.Fatalf("want 2 frames, got %d", len(got))
	}
	if got[0].Data != "no-space" || got[1].Data != "with-space" {
		t.Fatalf("got %+v", got)
	}
}

func TestReadSSE_EmptyInput(t *testing.T) {
	got := collectFrames(t, "")
	if len(got) != 0 {
		t.Fatalf("want 0 frames, got %d", len(got))
	}
}

func TestReadSSE_ContextCancellation(t *testing.T) {
	// Use a pipe so the reader blocks; cancel the context and expect
	// clean return.
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan SSEFrame, 4)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ReadSSE(ctx, pr, frames)
	}()

	// Let it block on the pipe, then cancel + close to unblock.
	cancel()
	_ = pw.Close()

	// Should terminate cleanly.
	if err := <-errCh; err != nil {
		t.Fatalf("ReadSSE returned error on cancel: %v", err)
	}
}
