package observability

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientActivityObservesRequestsWithoutInferringSessions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	activity := NewClientActivity(now)
	id := ClientIdentity{KeyID: "admin", Name: "Operator", Source: "startup", Protocol: "management-mcp", Authentication: "oauth"}
	first := activity.Start(id, now)
	second := activity.Start(id, now.Add(time.Second))
	rows, since, truncated := activity.Snapshot(now.Add(2 * time.Second))
	require.Equal(t, now, since)
	require.False(t, truncated)
	require.Len(t, rows, 1)
	require.Equal(t, 2, rows[0].ActiveRequests)
	require.EqualValues(t, 2, rows[0].RequestCount)
	first(now.Add(3 * time.Second))
	first(now.Add(4 * time.Second))
	second(now.Add(5 * time.Second))
	rows, _, _ = activity.Snapshot(now.Add(6 * time.Second))
	require.Zero(t, rows[0].ActiveRequests)
	require.Equal(t, now.Add(5*time.Second), rows[0].LastSeenAt)
	rows[0].Name = "edited snapshot"
	rows, _, _ = activity.Snapshot(now.Add(6 * time.Second))
	require.Equal(t, "Operator", rows[0].Name)
	rows, _, _ = activity.Snapshot(now.Add(ClientActivityRetention + time.Minute))
	require.Empty(t, rows)
}

func TestClientActivitySeparatesIdentitySourceProtocolAndAuthentication(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	activity := NewClientActivity(now)
	for _, identity := range []ClientIdentity{
		{KeyID: "admin", Source: "startup", Protocol: "management-mcp", Authentication: "api-key"},
		{KeyID: "admin", Source: "managed", Protocol: "management-mcp", Authentication: "api-key"},
		{KeyID: "admin", Source: "startup", Protocol: "management-api", Authentication: "api-key"},
		{KeyID: "admin", Source: "startup", Protocol: "management-mcp", Authentication: "oauth"},
	} {
		activity.Start(identity, now)(now)
	}
	rows, _, _ := activity.Snapshot(now)
	require.Len(t, rows, 4)
}

func TestClientActivityIsBoundedAndNeverEvictsInflightRequests(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	activity := NewClientActivity(now)
	finish := make([]func(time.Time), clientActivityLimit)
	for i := range finish {
		finish[i] = activity.Start(ClientIdentity{KeyID: fmt.Sprint(i)}, now)
	}
	activity.Start(ClientIdentity{KeyID: "overflow"}, now)(now)
	rows, _, truncated := activity.Snapshot(now.Add(2 * ClientActivityRetention))
	require.Len(t, rows, clientActivityLimit)
	require.True(t, truncated)
	finish[0](now.Add(2 * ClientActivityRetention))
	activity.Start(ClientIdentity{KeyID: "newest"}, now.Add(2*ClientActivityRetention))(now.Add(2 * ClientActivityRetention))
	rows, _, _ = activity.Snapshot(now.Add(2 * ClientActivityRetention))
	require.Len(t, rows, clientActivityLimit)
	for _, row := range rows {
		require.NotEqual(t, "0", row.KeyID)
		require.NotEqual(t, "overflow", row.KeyID)
	}
}

func TestClientActivityConcurrentRequestsAndSnapshots(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	activity := NewClientActivity(now)
	var workers sync.WaitGroup
	for range 30 {
		workers.Go(func() {
			for range 30 {
				finish := activity.Start(ClientIdentity{KeyID: "one"}, now)
				activity.Snapshot(now)
				finish(now)
			}
		})
	}
	workers.Wait()
	rows, _, _ := activity.Snapshot(now)
	require.Len(t, rows, 1)
	require.Zero(t, rows[0].ActiveRequests)
	require.EqualValues(t, 900, rows[0].RequestCount)
}

func TestClientActivityOutOfOrderStartsPreserveLatestObservation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	activity := NewClientActivity(now)
	recent := activity.Start(ClientIdentity{KeyID: "one", Name: "New name"}, now.Add(2*time.Second))
	older := activity.Start(ClientIdentity{KeyID: "one", Name: "Old name"}, now)
	older(now.Add(time.Second))
	rows, _, _ := activity.Snapshot(now.Add(3 * time.Second))
	require.Equal(t, "New name", rows[0].Name)
	require.Equal(t, now, rows[0].FirstSeenAt)
	require.Equal(t, now.Add(2*time.Second), rows[0].LastSeenAt)
	require.Equal(t, 1, rows[0].ActiveRequests)
	recent(now.Add(4 * time.Second))
}
