package retry

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"
)

func TestBackoff_ZeroValueProducesZeroDelay(t *testing.T) {
	t.Parallel()
	var b Backoff
	if d := b.Delay(0); d != 0 {
		t.Errorf("zero-value Backoff: want 0, got %v", d)
	}
}

func TestBackoff_ExponentialGrowth(t *testing.T) {
	t.Parallel()
	b := Backoff{Initial: 10 * time.Millisecond, Max: time.Second, Multiplier: 2.0}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 10 * time.Millisecond},
		{1, 20 * time.Millisecond},
		{2, 40 * time.Millisecond},
		{3, 80 * time.Millisecond},
		{10, time.Second}, // capped at Max
	}
	for _, c := range cases {
		if got := b.Delay(c.attempt); got != c.want {
			t.Errorf("attempt=%d: want %v, got %v", c.attempt, c.want, got)
		}
	}
}

func TestBackoff_JitterWithinRange(t *testing.T) {
	t.Parallel()
	b := Backoff{Initial: 100 * time.Millisecond, Max: time.Second, Multiplier: 2.0, Jitter: 0.2}
	// attempt=1 → 200ms base, ±20% = [160ms, 240ms]
	// Floor also kicks in at Initial/2 = 50ms, which is below 160.
	min := 160 * time.Millisecond
	max := 240 * time.Millisecond
	// Sample many times to exercise the random path.
	for range 100 {
		d := b.Delay(1)
		if d < min || d > max {
			t.Errorf("jittered delay %v out of range [%v, %v]", d, min, max)
		}
	}
}

func TestBackoff_DefaultMultiplier(t *testing.T) {
	t.Parallel()
	b := Backoff{Initial: 10 * time.Millisecond, Max: time.Second} // Multiplier zero → default 2.0
	if d := b.Delay(2); d != 40*time.Millisecond {
		t.Errorf("default multiplier: want 40ms, got %v", d)
	}
}

func TestBackoff_Multiplier1IsConstant(t *testing.T) {
	t.Parallel()
	// Multiplier=1.0 is a constant-delay policy — useful for fixed-rate
	// polling. Every attempt should return Initial, regardless of count.
	b := Backoff{Initial: 10 * time.Millisecond, Max: time.Second, Multiplier: 1.0}
	for _, attempt := range []int{0, 1, 5, 100} {
		if d := b.Delay(attempt); d != 10*time.Millisecond {
			t.Errorf("mult=1 attempt=%d: want 10ms, got %v", attempt, d)
		}
	}
}

func TestBackoff_OverflowClamp(t *testing.T) {
	t.Parallel()
	// Huge attempt counts must not overflow into negative durations.
	// Should clamp at Max when Max is set.
	b := Backoff{Initial: time.Second, Max: time.Hour, Multiplier: 2.0}
	for _, attempt := range []int{62, 100, 1000, 1 << 30} {
		d := b.Delay(attempt)
		if d != time.Hour {
			t.Errorf("huge attempt=%d: want Max=1h, got %v", attempt, d)
		}
	}
}

func TestClassifyNetwork_Baseline(t *testing.T) {
	t.Parallel()
	c := ClassifyNetwork()

	cases := []struct {
		name string
		err  error
		want Decision
	}{
		{"nil", nil, Stop},
		{"canceled", context.Canceled, Stop},
		{"deadline", context.DeadlineExceeded, Stop},
		{"tls cert", &tls.CertificateVerificationError{Err: errors.New("x")}, Stop},
		{"dns nxdomain", &net.DNSError{IsNotFound: true}, Stop},
		{"dns transient", &net.DNSError{IsTimeout: true}, Retry},
		{"generic", errors.New("connection refused"), Retry},
	}
	for _, tc := range cases {
		if got := c(tc.err); got != tc.want {
			t.Errorf("%s: want %v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestClassifyNetwork_Sentinels(t *testing.T) {
	t.Parallel()
	errForbidden := errors.New("forbidden by policy")
	errRevoked := errors.New("cert revoked")
	c := ClassifyNetwork(errForbidden, errRevoked)

	// Each sentinel individually stops.
	for _, e := range []error{errForbidden, errRevoked} {
		if got := c(e); got != Stop {
			t.Errorf("sentinel %v: want Stop, got %v", e, got)
		}
	}
	// Wrapped sentinel also stops (errors.Is through %w).
	wrapped := errors.Join(errors.New("outer"), errForbidden)
	if got := c(wrapped); got != Stop {
		t.Errorf("wrapped sentinel: want Stop, got %v", got)
	}
	// Other errors still retry.
	if got := c(errors.New("something else")); got != Retry {
		t.Errorf("other err: want Retry, got %v", got)
	}
}

func TestClassifyNetwork_UnwrapsThroughFmtErrorf(t *testing.T) {
	t.Parallel()
	// Real-world case: connector wraps lower-level net.DNSError via
	// fmt.Errorf("...: %w", err) at the version-discovery boundary.
	// Classifier must unwrap to find the typed error.
	c := ClassifyNetwork()
	wrapped := errors.Join(errors.New("version discovery failed"), &net.DNSError{IsNotFound: true})
	if got := c(wrapped); got != Stop {
		t.Errorf("wrapped DNSError: want Stop, got %v", got)
	}
}

func TestSleep_HonorsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	if err := Sleep(ctx, time.Second); err == nil {
		t.Error("cancelled ctx: want ctx.Err(), got nil")
	}
}

func TestSleep_ZeroDuration(t *testing.T) {
	t.Parallel()
	if err := Sleep(context.Background(), 0); err != nil {
		t.Errorf("zero duration: want nil, got %v", err)
	}
}

func TestSleep_TimerFires(t *testing.T) {
	t.Parallel()
	start := time.Now()
	if err := Sleep(context.Background(), 20*time.Millisecond); err != nil {
		t.Errorf("unexpected err: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Errorf("timer fired too early: %v", elapsed)
	}
}
