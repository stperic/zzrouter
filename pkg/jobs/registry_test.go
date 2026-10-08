package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
)

func newTestRegistry(t *testing.T) (*Registry, *clocktest.FakeClock) {
	t.Helper()
	fc := clocktest.NewFakeClock(time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC))
	r, err := NewRegistry(Config{
		NodeName:         "test-node",
		Clock:            fc,
		CompletedTTL:     5 * time.Minute,
		JanitorInterval:  30 * time.Second,
		SubscriberBuffer: 8,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(r.Stop)
	return r, fc
}

func drain(ch <-chan Event) []Event {
	out := []Event{}
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestStartEmitsPending(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, err := r.Start(context.Background(), KindDownload, "admin", Meta{"model": "llama3"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(h.ID(), "dl_") {
		t.Errorf("ID = %q, want dl_ prefix", h.ID())
	}
	if h.Epoch() == "" {
		t.Error("epoch must be non-empty")
	}
	ev, err := r.Get(h.ID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ev.Phase != PhasePending {
		t.Errorf("phase = %v, want pending", ev.Phase)
	}
	if ev.Seq != 0 {
		t.Errorf("seq = %d, want 0", ev.Seq)
	}
	if ev.Meta["model"] != "llama3" {
		t.Errorf("meta lost: %v", ev.Meta)
	}
}

func TestProgressThenDone(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindInstall, "admin", nil)

	sub, err := r.Subscribe(h.ID(), SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	h.Progress(25, "downloading", Bytes{Done: 100, Total: 400})
	h.Progress(75, "extracting", Bytes{Done: 300, Total: 400})
	h.Done()

	events := drain(sub.Events())
	if len(events) < 4 {
		t.Fatalf("events = %d, want >= 4 (pending + 2 running + done)", len(events))
	}
	last := events[len(events)-1]
	if last.Phase != PhaseDone {
		t.Errorf("final phase = %v, want done", last.Phase)
	}
	if last.Percent != 100 {
		t.Errorf("done percent = %d, want 100", last.Percent)
	}
}

func TestFailCarriesError(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindSync, "admin", nil)
	sub, _ := r.Subscribe(h.ID(), SubscribeOptions{})
	h.Fail(errors.New("disk full"))
	events := drain(sub.Events())
	last := events[len(events)-1]
	if last.Phase != PhaseFailed {
		t.Errorf("phase = %v, want failed", last.Phase)
	}
	if last.Err != "disk full" {
		t.Errorf("err = %q, want 'disk full'", last.Err)
	}
}

func TestLateProgressIsNoOp(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	h.Done()
	// Must not panic; must not bump seq.
	h.Progress(50, "zombie", Bytes{})
	h.Fail(errors.New("also-zombie"))
	ev, _ := r.Get(h.ID())
	if ev.Phase != PhaseDone {
		t.Errorf("phase changed after terminal: %v", ev.Phase)
	}
}

func TestSubscribeReplayFromZero(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	h.Progress(10, "", Bytes{})
	h.Progress(20, "", Bytes{})

	sub, err := r.Subscribe(h.ID(), SubscribeOptions{From: u64(0)})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	h.Done()
	events := drain(sub.Events())
	// Expect pending(seq=0), running(1), running(2), done(3)
	wantSeqs := []uint64{0, 1, 2, 3}
	if len(events) != len(wantSeqs) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantSeqs), events)
	}
	for i, ev := range events {
		if ev.Seq != wantSeqs[i] {
			t.Errorf("events[%d].seq = %d, want %d", i, ev.Seq, wantSeqs[i])
		}
	}
}

func TestSubscribeDroppedMarkerOnGap(t *testing.T) {
	r, _ := newTestRegistry(t)
	// Small ring to force overflow.
	r.cfg.KindPolicy = map[Kind]RingPolicy{
		KindDownload: {Mode: RingBounded, Size: 3},
	}
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	for i := 1; i <= 10; i++ {
		h.Progress(i*10, "", Bytes{})
	}

	sub, err := r.Subscribe(h.ID(), SubscribeOptions{From: u64(2)})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	h.Done()
	events := drain(sub.Events())
	if len(events) == 0 || events[0].Dropped == nil {
		t.Fatalf("want dropped marker as first event, got %+v", events)
	}
	if events[0].Dropped.Since != 2 {
		t.Errorf("dropped.Since = %d, want 2", events[0].Dropped.Since)
	}
	if events[0].Dropped.Current == 0 {
		t.Error("dropped.Current must be non-zero")
	}
}

func TestFirehoseRejectsReplay(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindInferenceLog, "admin", nil)
	_, err := r.Subscribe(h.ID(), SubscribeOptions{From: u64(5)})
	if !errors.Is(err, ErrReplayUnsupported) {
		t.Errorf("err = %v, want ErrReplayUnsupported", err)
	}
}

func TestFirehoseFromNowWorks(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindInferenceLog, "admin", nil)
	// No FromSet → live-only.
	sub, err := r.Subscribe(h.ID(), SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	h.Progress(0, "chunk", Bytes{})
	h.Done()
	events := drain(sub.Events())
	// Firehose has no ring, so the pending event is NOT replayed — we
	// only see events emitted after Subscribe attached. But the
	// subscribe lock/emit interleaving depends on scheduling; the only
	// hard invariant is: stream ends with a terminal event OR closes
	// cleanly.
	if len(events) == 0 {
		t.Fatal("firehose subscriber saw zero events; want at least the terminal")
	}
	last := events[len(events)-1]
	if !last.Phase.IsTerminal() {
		t.Errorf("final phase = %v, want terminal", last.Phase)
	}
}

func TestEpochMismatch(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	_, err := r.Subscribe(h.ID(), SubscribeOptions{Epoch: "garbage"})
	if !errors.Is(err, ErrEpochMismatch) {
		t.Errorf("err = %v, want ErrEpochMismatch", err)
	}
}

func TestCancelFiresHandleContext(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	if err := r.Cancel(h.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-h.Context().Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancel did not propagate to handle context")
	}
}

// TestStart_CallerCtxCancelPropagates pins the existing Start contract:
// a sync caller's ctx cancel must cancel Handle.Context(). This is the
// load-bearing semantic for pkg/update/scheduler.ApplyNow and any future
// sync caller — do not regress on the StartDetached refactor.
func TestStart_CallerCtxCancelPropagates(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	h, err := r.Start(ctx, KindDownload, "admin", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	select {
	case <-h.Context().Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("caller ctx cancel did not propagate to Handle.Context()")
	}
}

// TestStartDetached_CallerCtxCancelDoesNotPropagate is the regression
// test for the bug class fixed in fa91f367 (KindRun, KindSync) and
// 167dd7df (KindInstall × 4). StartDetached intentionally severs the
// caller-ctx → Handle.Context link so async-202 paths can't be killed
// by gin.Context.Request.Context() cancellation when the response
// flushes microseconds after Start returns.
func TestStartDetached_CallerCtxCancelDoesNotPropagate(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, err := r.StartDetached(KindInstall, "admin", nil)
	if err != nil {
		t.Fatalf("StartDetached: %v", err)
	}
	// No caller ctx exists to cancel — but verify the handle stays
	// alive for a representative span (gin's response-flush window is
	// microseconds, so any span > 0 disproves the bug).
	select {
	case <-h.Context().Done():
		t.Fatal("StartDetached handle ctx fired without Stop or Cancel")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestStartDetached_StopCancelsHandle pins that Registry.Stop still
// cancels detached handles — shutdown is the only legitimate cancel
// vector for an async-202 job, and losing that would leak goroutines.
func TestStartDetached_StopCancelsHandle(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, err := r.StartDetached(KindUpdate, "admin", nil)
	if err != nil {
		t.Fatalf("StartDetached: %v", err)
	}
	r.Stop()
	select {
	case <-h.Context().Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Registry.Stop did not cancel detached Handle.Context()")
	}
}

// TestStartDetached_AfterStopReturnsErrRegistryStopped pins that the
// Stop-then-Start error path is preserved through the wrapper. Cheap
// insurance against a future inline that loses the registry-stopped
// gate.
func TestStartDetached_AfterStopReturnsErrRegistryStopped(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.Stop()
	if _, err := r.StartDetached(KindInstall, "admin", nil); !errors.Is(err, ErrRegistryStopped) {
		t.Fatalf("StartDetached after Stop: err = %v, want ErrRegistryStopped", err)
	}
}

// TestStartDetached_CancelStillWorks pins that explicit Cancel via the
// Registry continues to work for detached handles — operators must
// retain the kill-switch even when callers can't cancel via ctx.
func TestStartDetached_CancelStillWorks(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, err := r.StartDetached(KindRun, "admin", nil)
	if err != nil {
		t.Fatalf("StartDetached: %v", err)
	}
	if err := r.Cancel(h.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-h.Context().Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Cancel did not propagate to detached Handle.Context()")
	}
}

func TestStopEvictsEverything(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	sub, _ := r.Subscribe(h.ID(), SubscribeOptions{})
	r.Stop()

	// Subscriber channel must close; handle ctx must fire.
	select {
	case _, ok := <-sub.Events():
		if ok {
			// May receive pending event that was already buffered before Stop.
			// Drain and confirm close.
			for range sub.Events() {
			}
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("subscriber channel did not close on Stop")
	}
	select {
	case <-h.Context().Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("handle ctx did not fire on Stop")
	}
	if _, err := r.Get(h.ID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Stop: err = %v, want ErrNotFound", err)
	}
}

// Janitor tests drive sweep directly rather than racing the goroutine
// against FakeClock.Advance — the goroutine's wakeup is scheduler-timed,
// so Advance+wake+sweep ordering is nondeterministic. Sweep is the unit
// under test; the goroutine is covered indirectly by Stop-joins-it in
// other tests.

func TestSweepEvictsPastCompletedTTL(t *testing.T) {
	r, fc := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	h.Done()

	fc.Advance(2 * time.Minute)
	r.sweep()
	if _, err := r.Get(h.ID()); err != nil {
		t.Errorf("premature eviction: %v", err)
	}
	fc.Advance(4 * time.Minute) // total 6min > 5min TTL
	r.sweep()
	if _, err := r.Get(h.ID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected eviction past TTL, got err=%v", err)
	}
}

func TestSweepReapsInactive(t *testing.T) {
	fc := clocktest.NewFakeClock(time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC))
	r, err := NewRegistry(Config{
		NodeName:        "test-node",
		Clock:           fc,
		CompletedTTL:    5 * time.Minute,
		JanitorInterval: 1 * time.Hour, // effectively disabled for this test
		KindInactivity:  map[Kind]time.Duration{KindDownload: 1 * time.Minute},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(r.Stop)

	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	h.Progress(10, "", Bytes{})

	fc.Advance(30 * time.Second)
	h.Progress(20, "", Bytes{}) // activity refresh
	r.sweep()
	if ev, _ := r.Get(h.ID()); ev.Phase == PhaseFailed {
		t.Fatal("premature reap: still within inactivity window after Progress refresh")
	}

	fc.Advance(90 * time.Second) // >1min since last Progress
	r.sweep()
	ev, err := r.Get(h.ID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ev.Phase != PhaseFailed {
		t.Errorf("phase = %v, want failed (inactivity reap)", ev.Phase)
	}
}

func TestConcurrentProducerAndSubscribers(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)

	const numSubs = 10
	var wg sync.WaitGroup
	for i := 0; i < numSubs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, err := r.Subscribe(h.ID(), SubscribeOptions{})
			if err != nil {
				t.Errorf("Subscribe: %v", err)
				return
			}
			for range sub.Events() {
			}
		}()
	}

	// Producer emits a burst; all subscribers must terminate cleanly on Done.
	for i := 0; i < 100; i++ {
		h.Progress(i, "", Bytes{})
	}
	h.Done()

	waitCh := make(chan struct{})
	go func() { wg.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribers did not close after Done")
	}
}

func TestSubscriberDropOldest(t *testing.T) {
	r, _ := newTestRegistry(t)
	// Tiny buffer to force drops.
	r.cfg.SubscriberBuffer = 2
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	sub, _ := r.Subscribe(h.ID(), SubscribeOptions{})

	// Emit more events than the buffer — drops must register, no block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			h.Progress(i, "", Bytes{})
		}
		h.Done()
		close(done)
	}()
	<-done

	drain(sub.Events())
	if sub.Dropped() == 0 {
		t.Error("expected drops with buffer=2 and 100+ events, got 0")
	}
}

func TestListFilters(t *testing.T) {
	r, _ := newTestRegistry(t)
	for i := 0; i < 3; i++ {
		_, _ = r.Start(context.Background(), KindDownload, "admin", nil)
	}
	for i := 0; i < 2; i++ {
		_, _ = r.Start(context.Background(), KindInstall, "admin", nil)
	}
	if got := len(r.List("")); got != 5 {
		t.Errorf("all: %d, want 5", got)
	}
	if got := len(r.List(KindDownload)); got != 3 {
		t.Errorf("downloads: %d, want 3", got)
	}
	if got := len(r.List(KindInstall)); got != 2 {
		t.Errorf("installs: %d, want 2", got)
	}
}

func TestStartAfterStopFails(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.Stop()
	_, err := r.Start(context.Background(), KindDownload, "admin", nil)
	if !errors.Is(err, ErrRegistryStopped) {
		t.Errorf("err = %v, want ErrRegistryStopped", err)
	}
}

func TestMetaMergesIntoSubsequentEvents(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", Meta{"a": 1})
	h.Meta(Meta{"b": 2})
	sub, _ := r.Subscribe(h.ID(), SubscribeOptions{})
	h.Done()
	events := drain(sub.Events())
	last := events[len(events)-1]
	if last.Meta["a"] != 1 || last.Meta["b"] != 2 {
		t.Errorf("meta = %v, want {a:1, b:2}", last.Meta)
	}
}

// Ensure randomHex produces unique IDs within a burst.
func TestRandomHexUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 1024)
	for i := 0; i < 1024; i++ {
		h := randomHex(8)
		if _, dup := seen[h]; dup {
			t.Fatalf("collision after %d draws: %s", i, h)
		}
		seen[h] = struct{}{}
	}
}

// u64 is a test helper — returns a *uint64 literal for SubscribeOptions.From.
func u64(v uint64) *uint64 { return &v }

// TestSubscribeAfterTerminalWithinTTL asserts that a subscriber attaching
// after the job has terminated (but before janitor eviction) replays the
// full ring including the terminal event, then observes channel close.
func TestSubscribeAfterTerminalWithinTTL(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	h.Progress(50, "", Bytes{})
	h.Done()

	sub, err := r.Subscribe(h.ID(), SubscribeOptions{From: u64(0)})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	events := drain(sub.Events())
	if len(events) == 0 {
		t.Fatal("terminal-within-TTL subscriber saw zero events")
	}
	last := events[len(events)-1]
	if last.Phase != PhaseDone {
		t.Errorf("final phase = %v, want done", last.Phase)
	}
}

// TestConcurrentSubscribeDuringEmit stresses the replay-vs-live race.
// Correct implementation: every subscriber sees strictly monotonic seqs
// with no duplicates and no gaps. Historical bug (pre-fix): subscribe
// released the lock between replay collection and live registration,
// so a concurrent emit could deliver a live event with seq N before
// the replay loop pushed replay seq < N.
func TestConcurrentSubscribeDuringEmit(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)

	// Start a producer emitting continuously.
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.Progress(i%100, "", Bytes{})
		}
	}()

	// Concurrently attach subscribers that each check monotonicity.
	var wg sync.WaitGroup
	const numSubs = 20
	for i := 0; i < numSubs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, err := r.Subscribe(h.ID(), SubscribeOptions{From: u64(0)})
			if err != nil {
				t.Errorf("Subscribe: %v", err)
				return
			}
			var lastSeq int64 = -1
			for ev := range sub.Events() {
				if ev.Dropped != nil {
					// Dropped markers have seq == firstSeq; don't enforce strict +1.
					lastSeq = int64(ev.Seq) - 1
					continue
				}
				if int64(ev.Seq) <= lastSeq {
					t.Errorf("subscriber seq regression: got %d after %d", ev.Seq, lastSeq)
					return
				}
				lastSeq = int64(ev.Seq)
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	h.Done()
	wg.Wait()
}

// TestStopDuringInFlightSubscribe covers the race where Registry.Stop
// evicts all jobs between Subscribe's registry-lookup and the job's
// j.subscribe call. Correct: the returned subscriber observes immediate
// end-of-stream via a closed channel.
func TestStopDuringInFlightSubscribe(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Best-effort: attempt Subscribe; may succeed before or after Stop.
		sub, err := r.Subscribe(h.ID(), SubscribeOptions{})
		if err != nil {
			if !errors.Is(err, ErrRegistryStopped) && !errors.Is(err, ErrNotFound) {
				t.Errorf("Subscribe err = %v, want stopped/notfound or nil", err)
			}
			return
		}
		// If we got a subscriber, channel must close without blocking.
		select {
		case <-time.After(500 * time.Millisecond):
			t.Error("subscriber channel did not close after Stop")
		case <-sub.Events():
			// OK — either we saw the pending event or the channel closed.
		}
		for range sub.Events() {
		}
	}()

	r.Stop()
	wg.Wait()
}

// TestEpochMatchExplicit covers the "epoch hint present and correct" path.
func TestEpochMatchExplicit(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	sub, err := r.Subscribe(h.ID(), SubscribeOptions{Epoch: h.Epoch()})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	h.Done()
	events := drain(sub.Events())
	if len(events) == 0 {
		t.Fatal("no events with matching epoch")
	}
}

// TestUnsubscribeWhileProducerEmits asserts that Unsubscribe racing a
// producer does not deadlock and closes the subscriber cleanly.
func TestUnsubscribeWhileProducerEmits(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, _ := r.Start(context.Background(), KindDownload, "admin", nil)
	sub, _ := r.Subscribe(h.ID(), SubscribeOptions{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			h.Progress(i%100, "", Bytes{})
		}
	}()

	// Unsubscribe concurrently with the producer.
	go sub.Unsubscribe()

	// Drain the channel — must close within a reasonable window.
	timeout := time.After(2 * time.Second)
	for {
		select {
		case <-timeout:
			t.Fatal("subscriber channel did not close after Unsubscribe + producer work")
		case _, ok := <-sub.Events():
			if !ok {
				<-done
				return
			}
		}
	}
}

func TestResumeUsesStableIDWithFreshEpoch(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, err := r.StartDetached(KindUpdate, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := newTestRegistry(t)
	resumed, err := fresh.Resume(context.Background(), KindUpdate, "", h.ID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID() != h.ID() || resumed.Epoch() == h.Epoch() {
		t.Fatal("resume must retain ID and replace epoch")
	}
	if _, err := fresh.Resume(context.Background(), KindUpdate, "", h.ID(), nil); err == nil {
		t.Fatal("duplicate ID accepted")
	}
	for _, id := range []string{"", "up_123", "up_gggggggggggggggg", "dl_0123456789abcdef"} {
		if _, err := fresh.Resume(context.Background(), KindUpdate, "", id, nil); err == nil {
			t.Fatalf("invalid ID accepted: %q", id)
		}
	}
}
