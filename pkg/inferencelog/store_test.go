package inferencelog

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeEntry(id string, model string) LogEntry {
	return LogEntry{
		ID:        id,
		Timestamp: time.Now(),
		Model:     model,
		App:       "ollama",
		Status:    StatusSuccess,
		TokensIn:  10,
		TokensOut: 20,
		LatencyMs: 100,
	}
}

func TestStore_AddAndQuery(t *testing.T) {
	s := NewStore(5, 0)

	s.Add(makeEntry("1", "llama3"), nil)
	s.Add(makeEntry("2", "mistral"), nil)
	s.Add(makeEntry("3", "llama3"), nil)

	entries := s.Query(QueryFilter{Limit: 10})
	require.Len(t, entries, 3)
	assert.Equal(t, "3", entries[0].ID, "newest first")
	assert.Equal(t, "1", entries[2].ID, "oldest last")
}

func TestStore_RingBufferWrap(t *testing.T) {
	s := NewStore(3, 0)

	s.Add(makeEntry("1", "a"), nil)
	s.Add(makeEntry("2", "b"), nil)
	s.Add(makeEntry("3", "c"), nil)
	s.Add(makeEntry("4", "d"), nil) // overwrites "1"
	s.Add(makeEntry("5", "e"), nil) // overwrites "2"

	assert.Equal(t, 3, s.Len())

	entries := s.Query(QueryFilter{Limit: 10})
	require.Len(t, entries, 3)
	assert.Equal(t, "5", entries[0].ID)
	assert.Equal(t, "4", entries[1].ID)
	assert.Equal(t, "3", entries[2].ID)

	// Old entries should be gone
	_, found := s.Get("1")
	assert.False(t, found)
	_, found = s.Get("2")
	assert.False(t, found)
}

func TestStore_Get(t *testing.T) {
	s := NewStore(10, 0)
	s.Add(makeEntry("abc", "llama3"), nil)

	entry, ok := s.Get("abc")
	require.True(t, ok)
	assert.Equal(t, "abc", entry.ID)

	_, ok = s.Get("nonexistent")
	assert.False(t, ok)
}

func TestStore_QueryFilter_Model(t *testing.T) {
	s := NewStore(10, 0)
	s.Add(makeEntry("1", "llama3"), nil)
	s.Add(makeEntry("2", "mistral"), nil)
	s.Add(makeEntry("3", "llama3"), nil)

	entries := s.Query(QueryFilter{Model: "llama3", Limit: 10})
	require.Len(t, entries, 2)
	assert.Equal(t, "3", entries[0].ID)
	assert.Equal(t, "1", entries[1].ID)
}

func TestStore_QueryFilter_Status(t *testing.T) {
	s := NewStore(10, 0)

	e1 := makeEntry("1", "llama3")
	e1.Status = StatusSuccess
	s.Add(e1, nil)

	e2 := makeEntry("2", "llama3")
	e2.Status = StatusError
	s.Add(e2, nil)

	entries := s.Query(QueryFilter{Status: StatusError, Limit: 10})
	require.Len(t, entries, 1)
	assert.Equal(t, "2", entries[0].ID)
}

func TestStore_QueryFilter_Since(t *testing.T) {
	s := NewStore(10, 0)

	old := makeEntry("1", "llama3")
	old.Timestamp = time.Now().Add(-10 * time.Minute)
	s.Add(old, nil)

	recent := makeEntry("2", "llama3")
	recent.Timestamp = time.Now()
	s.Add(recent, nil)

	entries := s.Query(QueryFilter{Since: time.Now().Add(-5 * time.Minute), Limit: 10})
	require.Len(t, entries, 1)
	assert.Equal(t, "2", entries[0].ID)
}

func TestStore_QueryFilter_Offset(t *testing.T) {
	s := NewStore(10, 0)
	for i := range 5 {
		s.Add(makeEntry(fmt.Sprintf("%d", i), "llama3"), nil)
	}

	entries := s.Query(QueryFilter{Limit: 2, Offset: 2})
	require.Len(t, entries, 2)
	assert.Equal(t, "2", entries[0].ID)
	assert.Equal(t, "1", entries[1].ID)
}

func TestStore_Stop(t *testing.T) {
	s := NewStore(10, 0)

	s.Add(makeEntry("pre-stop", "llama3"), nil)
	s.Stop()

	// Add after close must be a silent no-op (store rejects new entries
	// but existing history stays readable).
	s.Add(makeEntry("after-close", "llama3"), nil)
	assert.Equal(t, 1, s.Len(), "post-stop Add must not append")
}

func TestStore_ConcurrentAccess(t *testing.T) {
	s := NewStore(100, 0)

	var wg sync.WaitGroup

	// Concurrent writers
	for w := range 10 {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for i := range 50 {
				s.Add(makeEntry(fmt.Sprintf("w%d-e%d", writer, i), "llama3"), nil)
			}
		}(w)
	}

	// Concurrent readers
	for range 5 {
		wg.Go(func() {
			for range 50 {
				_ = s.Query(QueryFilter{Limit: 10})
				_ = s.Len()
			}
		})
	}

	wg.Wait()

	assert.Equal(t, 100, s.Len(), "ring buffer should be at capacity")
}

func TestStore_DefaultLimit(t *testing.T) {
	s := NewStore(100, 0)
	for i := range 100 {
		s.Add(makeEntry(fmt.Sprintf("%d", i), "llama3"), nil)
	}

	// Default limit should be 50
	entries := s.Query(QueryFilter{})
	assert.Len(t, entries, 50)
}
