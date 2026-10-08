package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpendEventBus_PublishToOneSubscriber(t *testing.T) {
	bus := NewSpendEventBus()
	events, _, unsub := bus.Subscribe()
	defer unsub()

	go bus.OnBreach(quota.BreachEvent{Reason: "rpm_limit_exceeded", ScopeID: "alice"})

	select {
	case ev := <-events:
		assert.Equal(t, "rpm_limit_exceeded", ev.Reason)
		assert.Equal(t, "alice", ev.ScopeID)
	case <-time.After(time.Second):
		t.Fatal("expected event within 1s")
	}
}

func TestSpendEventBus_FanOutToMultipleSubscribers(t *testing.T) {
	bus := NewSpendEventBus()

	const N = 5
	events := make([]<-chan quota.BreachEvent, N)
	unsubs := make([]func(), N)
	for i := 0; i < N; i++ {
		events[i], _, unsubs[i] = bus.Subscribe()
	}
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	bus.OnBreach(quota.BreachEvent{Reason: "budget_exhausted", ScopeID: "team-x"})

	for i, ch := range events {
		select {
		case ev := <-ch:
			assert.Equal(t, "budget_exhausted", ev.Reason, "subscriber %d", i)
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive event", i)
		}
	}
}

func TestSpendEventBus_DropOnFullChannel(t *testing.T) {
	bus := NewSpendEventBus()
	_, dropped, unsub := bus.Subscribe()
	defer unsub()

	// Drop count is deterministic only as long as OnBreach stays
	// synchronous — every send happens on this goroutine, so the
	// channel fills to exactly bufferSize then every subsequent send
	// hits the default branch. If anyone ever moves OnBreach to a
	// goroutine-per-subscriber model, this assertion becomes racy.
	for i := 0; i < spendEventBufferSize+10; i++ {
		bus.OnBreach(quota.BreachEvent{Reason: "rpm_limit_exceeded", ScopeID: fmt.Sprintf("k%d", i)})
	}

	assert.Equal(t, int64(10), dropped.Load(), "exactly the overflow events should be counted as dropped")
}

func TestSpendEventBus_UnsubRemovesSubscriber(t *testing.T) {
	bus := NewSpendEventBus()
	_, _, unsub := bus.Subscribe()
	assert.Equal(t, 1, bus.SubscriberCount())
	unsub()
	assert.Equal(t, 0, bus.SubscriberCount())
}

func TestSpendEventBus_ConcurrentSafety(t *testing.T) {
	bus := NewSpendEventBus()
	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, unsub := bus.Subscribe()
			defer unsub()
			bus.OnBreach(quota.BreachEvent{Reason: "rpm_limit_exceeded"})
		}()
	}
	wg.Wait()
	assert.Equal(t, 0, bus.SubscriberCount(), "every Subscribe was paired with unsub")
}

// TestSpendEventsStream_ReceivesBreachEvent exercises the full HTTP
// path: open the stream, inject a breach via the bus, read the SSE
// frame, decode it. Uses the live test server's engine to keep the
// test honest about middleware + auth.
func TestSpendEventsStream_ReceivesBreachEvent(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	require.NotNil(t, server.spendEvents, "test node must expose the spend bus")

	httpServer := httptest.NewServer(server.engine)
	defer httpServer.Close()

	req, err := http.NewRequest("GET", httpServer.URL+"/zzrouter/v1/spend/events", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", TestAdminKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// Inject the breach AFTER the subscription is live. Busy-loop on
	// SubscriberCount instead of a fixed sleep so -race -count=20
	// can't lose the publish to a slow CI box.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && server.spendEvents.SubscriberCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		server.spendEvents.OnBreach(quota.BreachEvent{
			Time:    time.Now().UTC(),
			Scope:   quota.ScopeKey,
			ScopeID: "alice",
			Reason:  "budget_exhausted",
		})
	}()

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	var sawBreach bool
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		if line == "event:breach" || line == "event: breach" {
			sawBreach = true
		}
		if sawBreach && strings.HasPrefix(line, "data:") {
			var ev quota.BreachEvent
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &ev))
			assert.Equal(t, "budget_exhausted", ev.Reason)
			assert.Equal(t, "alice", ev.ScopeID)
			return
		}
	}
	t.Fatal("never received a breach event over the SSE stream")
}

// TestSpendEventsStream_ReceivesThresholdEvent pins the wire contract
// for budget_threshold_* events: an agent decoding a breach frame
// should see Reason="budget_threshold_50" AND ThresholdPercent=50 on
// the same event so it can branch on either.
func TestSpendEventsStream_ReceivesThresholdEvent(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	require.NotNil(t, server.spendEvents, "test node must expose the spend bus")

	httpServer := httptest.NewServer(server.engine)
	defer httpServer.Close()

	req, err := http.NewRequest("GET", httpServer.URL+"/zzrouter/v1/spend/events", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", TestAdminKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && server.spendEvents.SubscriberCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		server.spendEvents.OnBreach(quota.BreachEvent{
			Time:             time.Now().UTC(),
			Scope:            quota.ScopeKey,
			ScopeID:          "alice",
			Reason:           "budget_threshold_50",
			SpendLimitMicro:  1_000_000,
			SpendUsedMicro:   600_000,
			BudgetPeriod:     "monthly",
			ThresholdPercent: 50,
		})
	}()

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	var sawBreach bool
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		if line == "event:breach" || line == "event: breach" {
			sawBreach = true
		}
		if sawBreach && strings.HasPrefix(line, "data:") {
			var ev quota.BreachEvent
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &ev))
			assert.Equal(t, "budget_threshold_50", ev.Reason)
			assert.Equal(t, uint8(50), ev.ThresholdPercent)
			assert.Equal(t, int64(1_000_000), ev.SpendLimitMicro)
			assert.Equal(t, int64(600_000), ev.SpendUsedMicro)
			return
		}
	}
	t.Fatal("never received a threshold event over the SSE stream")
}

// TestSpendEventsStream_AdminKeyRequired pins that the stream is gated
// by admin auth. A request with no key 401s; a request with an invalid
// key 401s. Anonymous SSE would leak quota state.
func TestSpendEventsStream_AdminKeyRequired(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	httpServer := httptest.NewServer(server.engine)
	defer httpServer.Close()

	for _, key := range []string{"", "wrong"} {
		req, _ := http.NewRequest("GET", httpServer.URL+"/zzrouter/v1/spend/events", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.NotEqual(t, http.StatusOK, resp.StatusCode, "key=%q should be rejected", key)
	}
}
