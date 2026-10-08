package clusternode

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPairingStore_RecordThenWaitForApproved(t *testing.T) {
	s := NewPairingStore(time.Minute)
	defer s.Stop()

	if _, err := s.Record(&PairingRequest{
		Code:        "CODE-ABCD-EFGH-IJKL",
		Fingerprint: "fp-1",
		NodeName:    "worker-1",
		CSRPEM:      "-----BEGIN CSR-----",
		SANs:        []string{"worker-1.local"},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Spawn WaitFor before Accept. If it delivers before Accept runs,
	// something is badly wrong with the status machine.
	resultCh := make(chan *PairingResult, 1)
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r, err := s.WaitFor(ctx, "CODE-ABCD-EFGH-IJKL")
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- r
	}()

	// Small yield so the goroutine parks on the done channel.
	time.Sleep(20 * time.Millisecond)

	if _, err := s.Accept("CODE-ABCD-EFGH-IJKL",
		[]byte("signed-cert"), []byte("ca-cert"), "https://coord:9091"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	select {
	case r := <-resultCh:
		if string(r.SignedCertPEM) != "signed-cert" {
			t.Errorf("signed cert: got %q", r.SignedCertPEM)
		}
		if string(r.CACertPEM) != "ca-cert" {
			t.Errorf("CA cert: got %q", r.CACertPEM)
		}
		if r.CoordinatorURL != "https://coord:9091" {
			t.Errorf("coord url: got %q", r.CoordinatorURL)
		}
		if r.NodeName != "worker-1" {
			t.Errorf("node name: got %q", r.NodeName)
		}
	case err := <-errCh:
		t.Fatalf("WaitFor errored: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor did not return")
	}
}

func TestPairingStore_DuplicateFingerprint_DifferentCode(t *testing.T) {
	s := NewPairingStore(time.Minute)
	defer s.Stop()

	if _, err := s.Record(&PairingRequest{
		Code:        "CODE-A",
		Fingerprint: "fp-1",
		NodeName:    "w",
	}); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	_, err := s.Record(&PairingRequest{
		Code:        "CODE-B",
		Fingerprint: "fp-1",
		NodeName:    "w",
	})
	if !errors.Is(err, ErrPairingFingerprintConflict) {
		t.Fatalf("expected ErrPairingFingerprintConflict, got %v", err)
	}
}

func TestPairingStore_DuplicateFingerprint_SameCodeIsIdempotent(t *testing.T) {
	s := NewPairingStore(time.Minute)
	defer s.Stop()

	first, err := s.Record(&PairingRequest{
		Code:        "CODE-IDEMPOTENT",
		Fingerprint: "fp-1",
		NodeName:    "w",
	})
	if err != nil {
		t.Fatalf("first Record: %v", err)
	}
	second, err := s.Record(&PairingRequest{
		Code:        "CODE-IDEMPOTENT",
		Fingerprint: "fp-1",
		NodeName:    "w",
	})
	if err != nil {
		t.Fatalf("second Record: %v", err)
	}
	// Idempotent re-submit returns the same underlying entry so a
	// re-polling worker observes the same done channel.
	if first != second {
		t.Errorf("expected idempotent return, got fresh entry")
	}
}

func TestPairingStore_AcceptUnknownCode(t *testing.T) {
	s := NewPairingStore(time.Minute)
	defer s.Stop()

	_, err := s.Accept("NOT-A-REAL-CODE", []byte("c"), []byte("ca"), "url")
	if !errors.Is(err, ErrPairingCodeUnknown) {
		t.Fatalf("expected ErrPairingCodeUnknown, got %v", err)
	}
}

func TestPairingStore_AcceptIsAtomic_FirstWriterWins(t *testing.T) {
	s := NewPairingStore(time.Minute)
	defer s.Stop()

	if _, err := s.Record(&PairingRequest{
		Code:        "RACE-CODE",
		Fingerprint: "fp-race",
		NodeName:    "w",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	const n = 100
	var wg sync.WaitGroup
	var successes int32
	var alreadyAccepted int32
	var other int32
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Accept("RACE-CODE", []byte("c"), []byte("ca"), "url")
			switch {
			case err == nil:
				atomic.AddInt32(&successes, 1)
			case errors.Is(err, ErrPairingAlreadyAccepted):
				atomic.AddInt32(&alreadyAccepted, 1)
			default:
				// Once the entry is delivered via WaitFor it could
				// also return ErrPairingCodeUnknown; this test has
				// no WaitFor so that branch shouldn't fire, but
				// count it if it does so the failure is legible.
				atomic.AddInt32(&other, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Errorf("expected exactly 1 successful Accept, got %d (already=%d other=%d)",
			successes, alreadyAccepted, other)
	}
	if alreadyAccepted != n-1 {
		t.Errorf("expected %d already-accepted, got %d", n-1, alreadyAccepted)
	}
}

func TestPairingStore_TTLEviction(t *testing.T) {
	// Manual clock so we don't wait for wall time.
	var nowNanos atomic.Int64
	nowNanos.Store(time.Unix(0, 0).UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()) }

	s := newPairingStoreWithClock(50*time.Millisecond, clock)
	defer s.Stop()

	if _, err := s.Record(&PairingRequest{
		Code:        "EXPIRING-CODE",
		Fingerprint: "fp-exp",
		NodeName:    "w",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Advance clock past TTL, then call WaitFor (which evicts on read).
	nowNanos.Store(clock().Add(100 * time.Millisecond).UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := s.WaitFor(ctx, "EXPIRING-CODE")
	if !errors.Is(err, ErrPairingCodeExpired) {
		t.Fatalf("expected ErrPairingCodeExpired, got %v", err)
	}

	// Accept on an evicted code should be unknown (entry gone), not
	// expired — the eviction path deletes from indices.
	_, err = s.Accept("EXPIRING-CODE", []byte("c"), []byte("ca"), "url")
	if !errors.Is(err, ErrPairingCodeUnknown) {
		t.Fatalf("post-eviction Accept: expected ErrPairingCodeUnknown, got %v", err)
	}
}

func TestPairingStore_WaitForExpiresDuringWait(t *testing.T) {
	// Real clock but very short TTL — exercises the GC goroutine
	// closing done on a parked waiter.
	s := NewPairingStore(40 * time.Millisecond)
	defer s.Stop()

	if _, err := s.Record(&PairingRequest{
		Code:        "SLOW-CODE",
		Fingerprint: "fp-slow",
		NodeName:    "w",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := s.WaitFor(ctx, "SLOW-CODE")
	if !errors.Is(err, ErrPairingCodeExpired) {
		t.Fatalf("expected ErrPairingCodeExpired, got %v", err)
	}
}

func TestPairingStore_StopCancelsWaitFor(t *testing.T) {
	s := NewPairingStore(time.Minute)

	if _, err := s.Record(&PairingRequest{
		Code:        "STOP-CODE",
		Fingerprint: "fp-stop",
		NodeName:    "w",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := s.WaitFor(context.Background(), "STOP-CODE")
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	s.Stop()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrPairingStoreStopped) {
			t.Fatalf("expected ErrPairingStoreStopped, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor did not return after Stop")
	}

	// Post-Stop API calls fail cleanly.
	if _, err := s.Record(&PairingRequest{Code: "X", Fingerprint: "fpX"}); !errors.Is(err, ErrPairingStoreStopped) {
		t.Errorf("post-stop Record: expected ErrPairingStoreStopped, got %v", err)
	}
	if _, err := s.Accept("X", nil, nil, ""); !errors.Is(err, ErrPairingStoreStopped) {
		t.Errorf("post-stop Accept: expected ErrPairingStoreStopped, got %v", err)
	}
}

func TestPairingStore_Pending_RedactsCode(t *testing.T) {
	s := NewPairingStore(time.Minute)
	defer s.Stop()

	if _, err := s.Record(&PairingRequest{
		Code:        "VISIBLE-CODE-1",
		Fingerprint: "fp-aaa",
		NodeName:    "worker-a",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := s.Record(&PairingRequest{
		Code:        "VISIBLE-CODE-2",
		Fingerprint: "fp-bbb",
		NodeName:    "worker-b",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	summaries := s.Pending()
	if len(summaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(summaries))
	}
	// Struct shape: PendingSummary has no Code field (compile-time
	// guarantee of redaction). This assertion is belt-and-suspenders.
	for _, sum := range summaries {
		if sum.NodeName == "" || sum.Fingerprint == "" {
			t.Errorf("summary missing required fields: %+v", sum)
		}
		if sum.Age < 0 {
			t.Errorf("negative age: %v", sum.Age)
		}
	}
}

func TestPairingStore_AcceptWakesWaitFor(t *testing.T) {
	// Stress the wakeup path: many concurrent WaitFor + Accept pairs,
	// each on a distinct code. A missed wakeup would leave a waiter
	// parked past the test deadline.
	s := NewPairingStore(time.Second)
	defer s.Stop()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		code := "PAIR-" + string(rune('A'+(i%26))) + "-" + string(rune('0'+(i/26)))
		fp := "fp-" + code
		if _, err := s.Record(&PairingRequest{
			Code:        code,
			Fingerprint: fp,
			NodeName:    "w",
		}); err != nil {
			t.Fatalf("Record %s: %v", code, err)
		}

		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := s.WaitFor(ctx, c); err != nil {
				t.Errorf("WaitFor %s: %v", c, err)
			}
		}(code)
	}

	// Small delay so waiters park, then fire Accept in parallel.
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < n; i++ {
		code := "PAIR-" + string(rune('A'+(i%26))) + "-" + string(rune('0'+(i/26)))
		go func(c string) {
			if _, err := s.Accept(c, []byte("cert"), []byte("ca"), "url"); err != nil {
				t.Errorf("Accept %s: %v", c, err)
			}
		}(code)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("waiters did not wake")
	}
}

func TestPairingStore_WaitForUnknownCode(t *testing.T) {
	s := NewPairingStore(time.Minute)
	defer s.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := s.WaitFor(ctx, "NOBODY-RECORDED-THIS")
	if !errors.Is(err, ErrPairingCodeUnknown) {
		t.Fatalf("expected ErrPairingCodeUnknown, got %v", err)
	}
}

func TestPairingStore_WaitForCtxCancelPreservesEntry(t *testing.T) {
	s := NewPairingStore(time.Minute)
	defer s.Stop()

	if _, err := s.Record(&PairingRequest{
		Code:        "RECONNECT-CODE",
		Fingerprint: "fp-rc",
		NodeName:    "w",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// First WaitFor times out (simulates the 30s long-poll hold
	// expiring without an Accept).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.WaitFor(ctx, "RECONNECT-CODE"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}

	// Entry MUST survive — worker reconnects on the same code.
	if _, err := s.Accept("RECONNECT-CODE", []byte("c"), []byte("ca"), "url"); err != nil {
		t.Fatalf("Accept after ctx cancel: %v", err)
	}
}
