package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
)

// A worker launches from its own copy of the provider tree, and the
// coordinator pushes that tree asynchronously. So a parameter write
// that answered as soon as it had persisted described a config the
// workers might not have for another round trip: PATCH then launch,
// documented as read-your-writes, resolved the previous value.
//
// The write waits for the push now, and says which of the two happened.
func TestReportSyncState_SaysWhetherTheWorkersHaveIt(t *testing.T) {
	t.Run("every worker took it", func(t *testing.T) {
		e := (&ParamsExecutor{}).WithSyncWait(func(context.Context) SyncReport { return SyncReport{State: SyncComplete} })
		c, rec := testGinContext(http.MethodPatch, "/zzrouter/v1/providers/vllm/parameters")
		e.awaitSync.await(c)
		assert.Equal(t, "complete", rec.Header().Get(headerProviderSync))
	})

	t.Run("still in flight", func(t *testing.T) {
		e := (&ParamsExecutor{}).WithSyncWait(func(context.Context) SyncReport { return SyncReport{State: SyncPending} })
		c, rec := testGinContext(http.MethodPatch, "/zzrouter/v1/providers/vllm/parameters")
		e.awaitSync.await(c)
		assert.Equal(t, "pending", rec.Header().Get(headerProviderSync),
			"a write that outran its own push must not read as delivered")
	})

	// The case this lab hits daily: one worker asleep. The round
	// finishes, so a plain "did it quiesce" reads as success — and the
	// sleeping node goes on resolving launches from the old tree.
	t.Run("a worker was down", func(t *testing.T) {
		e := (&ParamsExecutor{}).WithSyncWait(func(context.Context) SyncReport { return SyncReport{State: SyncPartial} })
		c, rec := testGinContext(http.MethodPatch, "/zzrouter/v1/providers/vllm/parameters")
		e.awaitSync.await(c)
		assert.Equal(t, "partial", rec.Header().Get(headerProviderSync),
			"a round that left a worker behind must not read as complete")
	})

	// Unwired — a role that pushes to nobody, which in practice is only
	// a test. A real node always wires the wait, so the header is not
	// how a caller tells a standalone from a coordinator.
	t.Run("no wait wired", func(t *testing.T) {
		c, rec := testGinContext(http.MethodPatch, "/zzrouter/v1/providers/vllm/parameters")
		(&ParamsExecutor{}).awaitSync.await(c)
		assert.Empty(t, rec.Header().Get(headerProviderSync))
	})
}

// The header has to reach a real response, not just the helper: the
// wiring is what makes the promise, and it is one `.WithSyncWait` away
// from being silently absent.
func TestIntegration_ParameterWriteReportsPropagation(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)

	resp := makeAuthRequest(t, server, "PATCH", "/zzrouter/v1/providers/vllm/parameters", TestAdminKey,
		map[string]any{"defaults": map[string]any{"parameters": map[string]any{"max-model-len": 4096}}})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", string(resp.Body))
	// This node has no workers, so the round is trivially complete —
	// what is under test is that the header reaches the response at
	// all, which is one missing `.WithSyncWait` away from silence.
	assert.Equal(t, "complete", resp.Headers.Get(headerProviderSync))
}

// AwaitQuiesce must not report a push that has not happened. The
// listener fires inside the store mutation, so a writer reaching the
// wait always finds its own round already in flight.
func TestProviderSyncFanOut_AwaitQuiesceWaitsForTheRound(t *testing.T) {
	store := newFanOutTestStore(t)
	release := make(chan struct{})
	pushed := make(chan struct{}, 1)
	srv := newBlockingSyncServer(t, release, pushed)

	f := newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint { return []*mesh.Endpoint{{URL: srv.URL, NodeName: "w1"}} },
		func() *http.Client { return srv.Client() },
	)

	// Nothing has fired: there is nothing to wait for.
	assert.Equal(t, SyncComplete, f.AwaitQuiesce(t.Context()).State)

	f.Listener()(store.Config())
	<-pushed // the round is inside the push and cannot have finished

	early, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	assert.Equal(t, SyncPending, f.AwaitQuiesce(early).State,
		"quiesced while a push was still blocked in the worker's handler")

	close(release)
	done, cancel2 := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel2()
	assert.Equal(t, SyncComplete, f.AwaitQuiesce(done).State, "never quiesced after the push completed")
}

// newBlockingSyncServer answers a provider push only once release
// yields — a send lets exactly one push through, closing the channel
// lets every later one through — and signals each arrival on pushed.
func newBlockingSyncServer(t *testing.T, release <-chan struct{}, pushed chan<- struct{}) *httptest.Server {
	t.Helper()
	// A handler still parked on release when the test ends would hold
	// httptest.Server.Close, which waits for outstanding requests: a
	// failed assertion would hang the package rather than report. The
	// gate opens unconditionally at cleanup, before Close is called.
	over := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case pushed <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-over:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		close(over)
		srv.Close()
	})
	return srv
}

// A round that could not reach a worker still finishes, so quiescence
// alone would report the write as delivered to a node that is running
// on the previous tree — the same over-claim the header exists to end.
func TestProviderSyncFanOut_AWorkerLeftBehindIsNotComplete(t *testing.T) {
	store := newFanOutTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	f := newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint { return []*mesh.Endpoint{{URL: srv.URL, NodeName: "w1"}} },
		func() *http.Client { return srv.Client() },
	)
	f.Listener()(store.Config())

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	assert.Equal(t, SyncReport{State: SyncPartial}, f.AwaitQuiesce(ctx),
		"a worker that refused the push must not read as in step")
}

// Only a named worker that took every push is vouched for. One that
// refused is not, and neither is one not yet known by name, whose runs
// could not be matched to it if it were.
func TestProviderSyncFanOut_LandedNamesOnlyWorkersThatTookIt(t *testing.T) {
	store := newFanOutTestStore(t)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	unnamed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(ok.Close)
	t.Cleanup(refused.Close)
	t.Cleanup(unnamed.Close)

	f := newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint {
			return []*mesh.Endpoint{{URL: ok.URL, NodeName: "took"}, {URL: refused.URL, NodeName: "refused"}, {URL: unnamed.URL}}
		},
		func() *http.Client { return ok.Client() },
	)
	f.Listener()(store.Config())

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got := f.AwaitQuiesce(ctx)
	assert.Equal(t, SyncReport{State: SyncPartial, Landed: []string{"took"}}, got)
	assert.True(t, got.Reached("took"))
	assert.False(t, got.Reached("refused"))
}

// The reason run() is a loop rather than a recursive re-fire: a
// mutation that arrives while a push is in flight is owed a round of
// its own, and until that round has run, the tree the workers have is
// the older one. A waiter that is told "quiesced" at the boundary
// between the two rounds is told its write landed before the push
// carrying it has started.
//
// The waiter has to be blocked ACROSS the boundary to see that: one
// that arrives after the next round has begun holds the new channel
// and waits correctly no matter how the boundary was written.
func TestProviderSyncFanOut_AReFireOwedIsStillInFlight(t *testing.T) {
	store := newFanOutTestStore(t)
	release := make(chan struct{})
	pushed := make(chan struct{}, 8)
	srv := newBlockingSyncServer(t, release, pushed)

	f := newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint { return []*mesh.Endpoint{{URL: srv.URL, NodeName: "w1"}} },
		func() *http.Client { return srv.Client() },
	)

	f.Listener()(store.Config())
	<-pushed
	// Arrives mid-round: coalesced into a re-fire that is now owed.
	f.Listener()(store.Config())

	// Start waiting BEFORE the round boundary, the way a PATCH whose
	// mutation was coalesced does.
	woke := make(chan SyncState, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		woke <- f.AwaitQuiesce(ctx).State
	}()

	// Let round one finish. Round two starts and blocks on the gate.
	release <- struct{}{}
	<-pushed

	select {
	case state := <-woke:
		t.Fatalf("woke with %q at the round boundary: a write coalesced into the re-fire "+
			"would read as delivered before its own push ran", state)
	case <-time.After(250 * time.Millisecond):
	}

	close(release)
	select {
	case state := <-woke:
		assert.Equal(t, SyncComplete, state)
	case <-time.After(5 * time.Second):
		t.Fatal("never woke after every round completed")
	}
}

// A worker that hangs on the push holds the round open past the wait. The
// workers that already took it are still named: a sleeping box must not
// hide one that has the write. Only a round carrying the waiter's write
// is trusted for that, which a round started for it always does.
func TestProviderSyncFanOut_PendingNamesWorkersThatAlreadyTookIt(t *testing.T) {
	store := newFanOutTestStore(t)
	took := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(took.Close)
	release := make(chan struct{})
	hangs := newBlockingSyncServer(t, release, make(chan struct{}, 8))

	f := newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint {
			return []*mesh.Endpoint{{URL: took.URL, NodeName: "awake"}, {URL: hangs.URL, NodeName: "asleep"}}
		},
		func() *http.Client { return took.Client() },
	)
	f.Listener()(store.Config())

	var got SyncReport
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		got = f.AwaitQuiesce(ctx)
		return got.Reached("awake")
	}, 5*time.Second, 10*time.Millisecond, "the worker that took the push was never named")
	assert.Equal(t, SyncPending, got.State)
	assert.False(t, got.Reached("asleep"))
	close(release)
}
