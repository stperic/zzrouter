package sqlstore

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/audit"
)

func subjects(events []audit.Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Subject
	}
	return out
}

func TestAuditPage(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), Options{})
	events, next, err := s.AuditPage(t.Context(), "", 2)
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Empty(t, next, "an empty history has no next page")
	for i := range 5 {
		s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: fmt.Sprint("event-", i)})
	}
	// A commit without an event writes no row, and seq stays gapless.
	_, err = s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
	require.NoError(t, err)
	s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "event-5"})

	var pages [][]string
	cursor := ""
	for {
		events, next, err := s.AuditPage(t.Context(), cursor, 2)
		require.NoError(t, err)
		pages = append(pages, subjects(events))
		if next == "" {
			break
		}
		cursor = next
	}
	assert.Equal(t, [][]string{{"event-5", "event-4"}, {"event-3", "event-2"}, {"event-1", "event-0"}}, pages, "newest first, in whole pages")

	events, next, err = s.AuditPage(t.Context(), cursor, 6)
	require.NoError(t, err)
	assert.Equal(t, []string{"event-1", "event-0"}, subjects(events), "a last cursor reads only what is older")
	assert.Empty(t, next)

	_, _, err = s.AuditPage(t.Context(), "", 0)
	require.Error(t, err)
	_, _, err = s.AuditPage(t.Context(), "", maxAuditPage+1)
	require.Error(t, err)

	other := openStore(t, t.TempDir(), Options{})
	other.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "elsewhere"})
	other.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "elsewhere"})
	_, foreign, err := other.AuditPage(t.Context(), "", 1)
	require.NoError(t, err)
	require.NotEmpty(t, foreign)
	encoded := func(text string) string { return base64.RawURLEncoding.EncodeToString([]byte(text)) }
	for name, bad := range map[string]string{
		"another generation": foreign,
		"not base64":         "%%%",
		"not a cursor":       encoded("[]"),
		"no generation":      encoded(`{"b":3}`),
		"empty":              encoded(`{}`),
		"null":               encoded(`null`),
	} {
		_, _, err = s.AuditPage(t.Context(), bad, 2)
		require.ErrorIs(t, err, audit.ErrCursor, name)
	}
	require.NoError(t, s.Close())
	_, _, err = s.AuditPage(t.Context(), "", 2)
	require.ErrorIs(t, err, access.ErrClosed)
}

// TestAuditTableIsAppendOnly refuses every rewrite of history, REPLACE included.
func TestAuditTableIsAppendOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "first"})
	s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "second"})
	require.NoError(t, s.Close())
	db := rawDB(t, dir)
	for name, query := range map[string]string{
		"update":         "UPDATE audit_events SET subject = 'rewritten'",
		"delete":         "DELETE FROM audit_events",
		"replace":        "INSERT OR REPLACE INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (1, 0, 'x', 'a', 'rewritten', '')",
		"insert earlier": "INSERT INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (0, 0, 'x', 'a', 'earlier', '')",
	} {
		_, err := db.ExecContext(t.Context(), query)
		require.Error(t, err, name)
	}
	_, err := db.ExecContext(t.Context(), "INSERT INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (3, 0, 'x', 'a', 'third', '')")
	require.NoError(t, err, "appending still works")
	assert.Equal(t, []string{"first", "second", "third"}, subjects(storedEvents(t, dir)))
}

// TestAuditPageWhileAnotherProgramHoldsTheDatabase: a read another program's
// lock refuses past the busy timeout is ErrBusy, and reads work once it lets go.
func TestAuditPageWhileAnotherProgramHoldsTheDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Actor: "operator", Subject: "reader"})
	_, err := s.db.SQL().ExecContext(t.Context(), "PRAGMA busy_timeout = 50") // the store's one connection keeps it
	require.NoError(t, err)
	other := rawDB(t, dir)
	_, err = other.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	_, _, err = s.AuditPage(t.Context(), "", 10)
	require.ErrorIs(t, err, ErrBusy)
	assert.Equal(t, ErrBusy.Error(), err.Error(), "no database text")
	_, err = other.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
	events, _, err := s.AuditPage(t.Context(), "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"reader"}, subjects(events))
}
