package sqlstore

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
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
	events, next, err := s.AuditPage(t.Context(), audit.Filter{}, "", 2)
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
		events, next, err := s.AuditPage(t.Context(), audit.Filter{}, cursor, 2)
		require.NoError(t, err)
		pages = append(pages, subjects(events))
		if next == "" {
			break
		}
		cursor = next
	}
	assert.Equal(t, [][]string{{"event-5", "event-4"}, {"event-3", "event-2"}, {"event-1", "event-0"}}, pages, "newest first, in whole pages")

	events, next, err = s.AuditPage(t.Context(), audit.Filter{}, cursor, 6)
	require.NoError(t, err)
	assert.Equal(t, []string{"event-1", "event-0"}, subjects(events), "a last cursor reads only what is older")
	assert.Empty(t, next)

	_, _, err = s.AuditPage(t.Context(), audit.Filter{}, "", 0)
	require.Error(t, err)
	_, _, err = s.AuditPage(t.Context(), audit.Filter{}, "", audit.MaxPage+1)
	require.Error(t, err)

	other := openStore(t, t.TempDir(), Options{})
	other.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "elsewhere"})
	other.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "elsewhere"})
	_, foreign, err := other.AuditPage(t.Context(), audit.Filter{}, "", 1)
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
		_, _, err = s.AuditPage(t.Context(), audit.Filter{}, bad, 2)
		require.ErrorIs(t, err, audit.ErrCursor, name)
	}
	require.NoError(t, s.Close())
	_, _, err = s.AuditPage(t.Context(), audit.Filter{}, "", 2)
	require.ErrorIs(t, err, access.ErrClosed)
}

// TestAuditTableIsAppendOnly refuses every rewrite of history, REPLACE
// included, and a row earlier than the newest, so time follows seq.
func TestAuditTableIsAppendOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "first"})
	s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "second"})
	require.NoError(t, s.Close())
	db := rawDB(t, dir)
	const newest = "(SELECT max(time) FROM audit_events)"
	for name, query := range map[string]string{
		"update":          "UPDATE audit_events SET subject = 'rewritten'",
		"delete":          "DELETE FROM audit_events",
		"replace":         "INSERT OR REPLACE INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (1, " + newest + ", 'x', 'a', 'rewritten', '')",
		"insert before":   "INSERT INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (0, " + newest + ", 'x', 'a', 'earlier', '')",
		"an earlier time": "INSERT INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (3, " + newest + " - 1, 'x', 'a', 'earlier', '')",
	} {
		_, err := db.ExecContext(t.Context(), query)
		require.ErrorContains(t, err, "audit events are append-only", name)
	}
	_, err := db.ExecContext(t.Context(), "INSERT INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (3, "+newest+", 'x', 'a', 'third', '')")
	require.NoError(t, err, "appending at the newest time still works")
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
	_, _, err = s.AuditPage(t.Context(), audit.Filter{}, "", 10)
	require.ErrorIs(t, err, ErrBusy)
	assert.Equal(t, ErrBusy.Error(), err.Error(), "no database text")
	_, err = other.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
	events, _, err := s.AuditPage(t.Context(), audit.Filter{}, "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"reader"}, subjects(events))
}

// TestAuditFilterQueryPlans: in a history of many events, the query AuditPage
// runs walks one index in seq order, the most selective equality's or the
// table's, bounded by a seq range that time resolves with seeks on audit_time:
// no scan and no sort for any filter. Not parallel: it sets the clock.
func TestAuditFilterQueryPlans(t *testing.T) {
	s := openStore(t, t.TempDir(), Options{})
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	record := func(at time.Time, ev audit.Event) {
		defer utils.SetClock(utils.FixedClock(at))()
		s.Record(t.Context(), ev)
	}
	record(base, audit.Event{Action: audit.ActionPasswordChanged, Actor: "rare", Subject: "rare"})
	for i := range 100 {
		record(base.Add(time.Duration(i+1)*time.Minute), audit.Event{Action: audit.ActionKeyUsageReset, Actor: "bulk", Subject: fmt.Sprint("bulk-", i)})
	}
	last, second := base.Add(100*time.Minute), base.Add(time.Minute)
	for _, tc := range []struct {
		walk   string
		filter audit.Filter
		newest string // the first page of one event; only the unfiltered history has more
	}{
		{"INTEGER PRIMARY KEY (rowid<?)", audit.Filter{}, "bulk-99"},
		{"INDEX audit_actor (actor=? AND seq<?)", audit.Filter{Actor: "rare"}, "rare"},
		{"INDEX audit_subject (subject=? AND seq<?)", audit.Filter{Subject: "rare"}, "rare"},
		{"INDEX audit_action (action=? AND seq<?)", audit.Filter{Action: audit.ActionPasswordChanged}, "rare"},
		{"INTEGER PRIMARY KEY (rowid>? AND rowid<?)", audit.Filter{Since: &last}, "bulk-99"},
		{"INTEGER PRIMARY KEY (rowid<?)", audit.Filter{Until: &second}, "rare"},
		{"INTEGER PRIMARY KEY (rowid>? AND rowid<?)", audit.Filter{Since: &base, Until: &second}, "rare"},
		{"INDEX audit_actor (actor=? AND seq<?)", audit.Filter{Actor: "rare", Action: audit.ActionPasswordChanged}, "rare"},
		{"INDEX audit_subject (subject=? AND seq<?)", audit.Filter{Subject: "rare", Action: audit.ActionPasswordChanged}, "rare"},
		{"INDEX audit_actor (actor=? AND seq>? AND seq<?)", audit.Filter{Actor: "bulk", Since: &last}, "bulk-99"},
		{"INDEX audit_subject (subject=? AND seq>? AND seq<?)", audit.Filter{Subject: "rare", Actor: "rare", Action: audit.ActionPasswordChanged, Since: &base, Until: &second}, "rare"},
	} {
		query, args := newAuditQuery(tc.filter).page(math.MaxInt64, 1)
		// The plan does not show which upper bound the walk seeks to; with one there is no choice.
		assert.Equal(t, 1, strings.Count(query, "seq < "), "%+v: one upper bound: %s", tc.filter, query)
		rows, err := s.db.SQL().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		require.NoError(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.NotEmpty(t, plan)
		assert.Equal(t, "SEARCH audit_events USING "+tc.walk, plan[0], "%+v: %v", tc.filter, plan)
		for _, step := range plan[1:] {
			if !strings.HasPrefix(step, "SCALAR SUBQUERY ") {
				assert.Equal(t, "SEARCH audit_events USING COVERING INDEX audit_time (time>?)", step, "%+v: %v", tc.filter, plan)
			}
		}
		events, next, err := s.AuditPage(t.Context(), tc.filter, "", 1)
		require.NoError(t, err)
		assert.Equal(t, []string{tc.newest}, subjects(events), "%+v", tc.filter)
		assert.Equal(t, tc.walk == "INTEGER PRIMARY KEY (rowid<?)" && tc.filter.Until == nil, next != "", "%+v", tc.filter)
	}
}

// TestAuditFilterDigest binds a cursor to the filter's values as the query
// compares them, never to how the query is written: the pinned digest changes
// only with that encoding, which would invalidate every open cursor.
func TestAuditFilterDigest(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	zoned := at.In(time.FixedZone("UTC-5", -5*3600))
	later := at.Add(time.Hour)
	f := audit.Filter{Actor: "alice", Subject: "eng", Action: audit.ActionKeyCreated, Since: &at, Until: &later}
	assert.Equal(t, "1aDFAYE_1JD-M_Qf5MzeY__HF8WNesg9dx3NTEaMHko", filterDigest(f))
	same := f
	same.Since = &zoned
	assert.Equal(t, filterDigest(f), filterDigest(same), "one instant in two zones")
	assert.Empty(t, filterDigest(audit.Filter{}), "no filter keeps the unfiltered cursor form")
	seen := map[string]audit.Filter{}
	for _, other := range []audit.Filter{
		{Actor: "eng"}, {Subject: "eng"}, {Action: "eng"}, {Since: &at}, {Until: &at}, {Since: &at, Until: &later}, f,
		{Actor: "alice", Subject: "eng", Action: audit.ActionKeyCreated, Since: &at},
	} {
		digest := filterDigest(other)
		_, dup := seen[digest]
		assert.False(t, dup, "%+v shares a digest with %+v", other, seen[digest])
		seen[digest] = other
	}
}
