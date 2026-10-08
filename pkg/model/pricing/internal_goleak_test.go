package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestLifecycle_Store_NoGoroutineLeak exercises Store Start→Stop and
// fails if the refresh-loop goroutine outlives the test.
//
// Store already uses the done-channel pattern (Stop blocks on `<-done`
// closed by `defer close(done)` in runRefreshLoop), so this test is a
// regression guard rather than a new-bug finder.
func TestLifecycle_Store_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	// Minimal httptest server so the initial refresh has something to
	// hit; refresh interval is long enough that the ticker never fires
	// during the test, so exit is entirely driven by Stop.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	store := NewStore(&testConfig{
		source:          server.URL,
		refreshInterval: 1 * time.Hour,
	}, t.TempDir())

	store.Start(context.Background())
	// Give Start's initial fetch time to complete so Stop doesn't race
	// the sync.Once disk-load path.
	time.Sleep(50 * time.Millisecond)
	store.Stop()
}
