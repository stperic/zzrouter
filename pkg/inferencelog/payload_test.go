package inferencelog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPayload_DropsNothingWorthKeeping(t *testing.T) {
	assert.Nil(t, NewPayload(nil, nil, nil))
	assert.Nil(t, NewPayload([]byte("not json"), nil, nil))
}

func TestNewPayload_OversizeDroppedNotTruncated(t *testing.T) {
	// Prose, not base64 — elision cannot shrink it, so the cap applies.
	huge := []byte(`{"prompt":"` + strings.Repeat("word ", MaxPayloadBytes/5+1) + `"}`)

	p := NewPayload(huge, nil, nil)

	require.NotNil(t, p)
	assert.True(t, p.Oversize)
	assert.Nil(t, p.Request, "oversize bodies are dropped so retained payloads stay valid JSON")
}

func TestNewPayload_OmitsUpstreamWhenIdentical(t *testing.T) {
	body := []byte(`{"model":"llama3"}`)

	same := NewPayload(body, body, nil)
	require.NotNil(t, same)
	assert.JSONEq(t, string(body), string(same.Request))
	assert.Nil(t, same.Upstream)

	rewritten := NewPayload(body, []byte(`{"model":"llama3:8b"}`), nil)
	require.NotNil(t, rewritten)
	assert.JSONEq(t, `{"model":"llama3:8b"}`, string(rewritten.Upstream))
}

func TestNewPayload_CopiesCallerBytes(t *testing.T) {
	body := []byte(`{"model":"llama3"}`)
	p := NewPayload(body, nil, nil)
	require.NotNil(t, p)

	body[2] = 'X' // caller reuses its buffer

	assert.JSONEq(t, `{"model":"llama3"}`, string(p.Request))
}

func TestStore_PayloadWindowShorterThanEntryWindow(t *testing.T) {
	s := NewStore(5, 2)

	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("%d", i)
		require.True(t, s.Add(makeEntry(id, "llama3"), NewPayload([]byte(`{"model":"llama3"}`), nil, nil)))
	}

	assert.Equal(t, 4, s.Len(), "metadata survives the payload window")
	assert.Equal(t, 2, s.PayloadLen())

	_, ok := s.Payload("2")
	assert.False(t, ok, "payload aged out of the shorter window")
	_, ok = s.Payload("4")
	assert.True(t, ok)

	aged, found := s.Get("2")
	require.True(t, found)
	assert.False(t, aged.PayloadAvailable)

	live, found := s.Get("4")
	require.True(t, found)
	assert.True(t, live.PayloadAvailable)
}

func TestStore_PayloadFlagOnQuery(t *testing.T) {
	s := NewStore(5, 1)
	s.Add(makeEntry("no-payload", "llama3"), nil)
	s.Add(makeEntry("with-payload", "llama3"), NewPayload([]byte(`{"a":1}`), nil, nil))

	entries := s.Query(QueryFilter{})
	require.Len(t, entries, 2)
	assert.True(t, entries[0].PayloadAvailable, "newest entry retains its payload")
	assert.False(t, entries[1].PayloadAvailable)
}

func TestStore_RetentionDisabled(t *testing.T) {
	s := NewStore(5, 0)

	assert.False(t, s.Add(makeEntry("1", "llama3"), NewPayload([]byte(`{"a":1}`), nil, nil)))
	assert.Equal(t, 0, s.PayloadLen())
	_, ok := s.Payload("1")
	assert.False(t, ok)

	entry, found := s.Get("1")
	require.True(t, found)
	assert.False(t, entry.PayloadAvailable)
}

func TestStore_PayloadWindowClampedToEntryWindow(t *testing.T) {
	s := NewStore(2, 100)

	for i := 1; i <= 3; i++ {
		s.Add(makeEntry(fmt.Sprintf("%d", i), "llama3"), NewPayload([]byte(`{"a":1}`), nil, nil))
	}

	assert.Equal(t, 2, s.PayloadLen(), "payloads cannot outlive the entries they belong to")
}

// The rings advance on different events: entries on every Add, payloads
// only on payload-carrying ones. Payload-less adds must still push a
// payload out with the entry it describes.
func TestStore_PayloadDiesWithItsEntry(t *testing.T) {
	s := NewStore(3, 3)

	s.Add(makeEntry("with-payload", "llama3"), NewPayload([]byte(`{"a":1}`), nil, nil))
	for _, id := range []string{"b", "c", "d"} {
		s.Add(makeEntry(id, "llama3"), nil)
	}

	_, found := s.Get("with-payload")
	require.False(t, found, "the entry was pushed out of the metadata ring")
	_, ok := s.Payload("with-payload")
	assert.False(t, ok, "its payload must not survive it")
	assert.Equal(t, 0, s.PayloadLen())
}

func TestStore_StopReleasesBodies(t *testing.T) {
	s := NewStore(5, 5)
	s.Add(makeEntry("1", "llama3"), NewPayload([]byte(`{"a":1}`), nil, nil))
	require.Equal(t, 1, s.PayloadLen())

	s.Stop()

	assert.Equal(t, 0, s.PayloadLen(), "prompt content must not outlive the store")
	_, ok := s.Payload("1")
	assert.False(t, ok)
	assert.False(t, s.Add(makeEntry("2", "llama3"), NewPayload([]byte(`{"a":1}`), nil, nil)))
}

// An oversize marker is retained to explain itself, but there is no body
// to advertise on the list surface.
func TestStore_OversizeMarkerIsNotAdvertised(t *testing.T) {
	s := NewStore(5, 5)
	huge := []byte(`{"prompt":"` + strings.Repeat("word ", MaxPayloadBytes/5+1) + `"}`)

	assert.False(t, s.Add(makeEntry("1", "llama3"), NewPayload(huge, nil, nil)))

	entry, found := s.Get("1")
	require.True(t, found)
	assert.False(t, entry.PayloadAvailable)

	payload, ok := s.Payload("1")
	require.True(t, ok, "the marker is still fetchable so the gap is explained")
	assert.True(t, payload.Oversize)
}

// Store is exported, so an empty ID must not read as a retained payload.
func TestStore_EmptyIDIsNotAPayload(t *testing.T) {
	s := NewStore(5, 5)
	s.Add(makeEntry("", "llama3"), nil)

	entry, found := s.Get("")
	require.True(t, found)
	assert.False(t, entry.PayloadAvailable)
	_, ok := s.Payload("")
	assert.False(t, ok)
}
