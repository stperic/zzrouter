package signal

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func workerExchange(t *testing.T) *Signaler {
	t.Helper()
	s := New(Config{IsWorker: func() bool { return true }})
	s.Start(t.Context())
	t.Cleanup(func() { s.Stop(context.Background()) })
	return s
}
func TestCatalogWaitsForPublicationAck(t *testing.T) {
	s := workerExchange(t)
	done := make(chan error, 1)
	go func() { done <- s.NotifyCacheRefresh(t.Context()) }()
	batch, err := s.Wait(t.Context(), Poll{})
	require.NoError(t, err)
	require.Len(t, batch.Operations, 1)
	select {
	case <-done:
		t.Fatal("delivery is not publication")
	default:
	}
	ack := Acknowledgment{Incarnation: "old", Results: []Result{{ID: batch.Operations[0].ID}}}
	require.False(t, s.Acknowledge(ack))
	select {
	case <-done:
		t.Fatal("old incarnation completed mutation")
	default:
	}
	ack.Incarnation = batch.Incarnation
	require.True(t, s.Acknowledge(ack))
	require.True(t, s.Acknowledge(ack))
	require.NoError(t, <-done)
}
func TestExchangeReplaysAndPreservesRefusal(t *testing.T) {
	s := workerExchange(t)
	done := make(chan error, 1)
	go func() { done <- s.RecordProviderEnabled(t.Context(), "comfyui", true) }()
	first, err := s.Wait(t.Context(), Poll{})
	require.NoError(t, err)
	replay, err := s.Wait(t.Context(), Poll{})
	require.NoError(t, err)
	require.Equal(t, first, replay)
	op := first.Operations[0]
	require.Equal(t, ProviderEnabled, op.Kind)
	require.Equal(t, "comfyui", op.Provider)
	require.True(t, op.Enabled)
	refusal := &StatusError{Status: 503, Code: "provider_sync_pending", Detail: "pending sync"}
	require.True(t, s.Acknowledge(Acknowledgment{Incarnation: first.Incarnation, Results: []Result{{ID: op.ID, Error: refusal}}}))
	var got *StatusError
	require.ErrorAs(t, <-done, &got)
	require.Equal(t, refusal, got)
}
func TestExchangeCancellationAndStop(t *testing.T) {
	s := workerExchange(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.NotifyCacheRefresh(ctx) }()
	batch, err := s.Wait(t.Context(), Poll{})
	require.NoError(t, err)
	require.Len(t, batch.Operations, 1)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	s.mu.Lock()
	require.Empty(t, s.pending)
	s.mu.Unlock()
	stopped := make(chan error, 1)
	go func() { _, err := s.Wait(t.Context(), Poll{}); stopped <- err }()
	s.Stop(t.Context())
	require.Error(t, <-stopped)
}
func TestAsyncNoticeNeverSatisfiesLaterMutation(t *testing.T) {
	s := workerExchange(t)
	s.NotifyCacheRefreshAsync(t.Context())
	old, err := s.Wait(t.Context(), Poll{})
	require.NoError(t, err)
	require.Len(t, old.Operations, 1)
	done := make(chan error, 1)
	go func() { done <- s.NotifyCacheRefresh(t.Context()) }()
	newer, err := s.Wait(t.Context(), Poll{Incarnation: old.Incarnation, Seen: []string{old.Operations[0].ID}})
	require.NoError(t, err)
	require.Len(t, newer.Operations, 1)
	require.True(t, s.Acknowledge(Acknowledgment{Incarnation: old.Incarnation, Results: []Result{{ID: old.Operations[0].ID}}}))
	select {
	case <-done:
		t.Fatal("older publication completed a later mutation")
	default:
	}
	require.True(t, s.Acknowledge(Acknowledgment{Incarnation: newer.Incarnation, Results: []Result{{ID: newer.Operations[0].ID}}}))
	require.NoError(t, <-done)
}
func TestRenewalResultUsesSameExchange(t *testing.T) {
	s := workerExchange(t)
	done := make(chan string, 1)
	go func() {
		cert, err := s.RenewCertificate(t.Context(), "csr")
		if err == nil {
			done <- cert
		} else {
			done <- err.Error()
		}
	}()
	batch, err := s.Wait(t.Context(), Poll{})
	require.NoError(t, err)
	require.Equal(t, Renew, batch.Operations[0].Kind)
	require.True(t, s.Acknowledge(Acknowledgment{Incarnation: batch.Incarnation, Results: []Result{{ID: batch.Operations[0].ID, Certificate: "signed"}}}))
	require.Equal(t, "signed", <-done)
}
