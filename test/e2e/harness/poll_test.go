package harness

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPollUntil_DoneFirstTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	err := PollUntil(ctx, time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return true, nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 {
		t.Fatalf("want 1 probe call, got %d", calls)
	}
}

func TestPollUntil_TerminalShortCircuits(t *testing.T) {
	want := errors.New("boom")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := PollUntil(ctx, time.Millisecond, func(context.Context) (bool, error) {
		return false, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("want %v, got %v", want, err)
	}
}

func TestPollUntil_CtxDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := PollUntil(ctx, 10*time.Millisecond, func(context.Context) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestPollUntil_DoneAfterTicks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	err := PollUntil(ctx, time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return calls >= 3, nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}
