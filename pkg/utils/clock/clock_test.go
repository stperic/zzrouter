package clock_test

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils/clock"
)

func TestSystemNowAndSinceAdvance(t *testing.T) {
	t.Parallel()
	c := clock.System()
	t0 := c.Now()
	time.Sleep(2 * time.Millisecond)
	if c.Since(t0) <= 0 {
		t.Fatalf("Since(t0) = %v, want > 0", c.Since(t0))
	}
}

func TestSystemAfter(t *testing.T) {
	t.Parallel()
	c := clock.System()
	select {
	case <-c.After(5 * time.Millisecond):
	case <-time.After(200 * time.Millisecond):
		t.Fatal("system After did not fire within 200ms")
	}
}

func TestSystemTickerFireAndStop(t *testing.T) {
	t.Parallel()
	c := clock.System()
	tk := c.NewTicker(5 * time.Millisecond)
	select {
	case <-tk.Chan():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("system ticker did not fire within 200ms")
	}
	tk.Stop()
}
