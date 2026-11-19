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
		require.NoError(t, s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: fmt.Sprint("event-", i)}))
	}
	// A commit without an event writes no row, and seq stays gapless.
	_, err = s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
	require.NoError(t, err)
	require.NoError(t, s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "event-5"}))

	var pages [][]string
	cursor := ""
	for {
		events, next, err := s.AuditPage(t.Context(), audit.Filter{}, cursor, 2)
		require.NoError(t, err)
		pages = append(pages, subjects(events))
		require.Less(t, len(pages), 4, "paging ends")
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
	require.NoError(t, other.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "elsewhere"}))
	require.NoError(t, other.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "elsewhere"}))
	_, foreign, err := other.AuditPage(t.Context(), audit.Filter{}, "", 1)
	require.NoError(t, err)
	require.NotEmpty(t, foreign)
	encoded := func(text string) string { return base64.RawURLEncoding.EncodeToString([]byte(text)) }
	generation := s.Meta().Generation
	for name, bad := range map[string]string{
		"another generation": foreign,
		"not base64":         "%%%",
		"not a cursor":       encoded("[]"),
		"no generation":      encoded(`{"t":3,"s":3}`),
		"the seq-only form":  encoded(fmt.Sprintf(`{"g":%q,"b":3}`, generation)),
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
// included. A row may carry an earlier time than the newest: a clock steps back.
func TestAuditTableIsAppendOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	require.NoError(t, s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "first"}))
	require.NoError(t, s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Subject: "second"}))
	require.NoError(t, s.Close())
	db := rawDB(t, dir)
	for name, query := range map[string]string{
		"update":        "UPDATE audit_events SET subject = 'rewritten'",
		"delete":        "DELETE FROM audit_events",
		"replace":       "INSERT OR REPLACE INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (1, 0, 'x', 'a', 'rewritten', '')",
		"insert before": "INSERT INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (2, 0, 'x', 'a', 'earlier', '')",
	} {
		_, err := db.ExecContext(t.Context(), query)
		require.ErrorContains(t, err, "audit events are append-only", name)
	}
	_, err := db.ExecContext(t.Context(), "INSERT INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (3, 0, 'x', 'a', 'third', '')")
	require.NoError(t, err, "appending works, with any time")
	assert.Equal(t, []string{"first", "second", "third"}, subjects(storedEvents(t, dir)))
}

// TestAuditPageWhileAnotherProgramHoldsTheDatabase: a read another program's
// lock refuses past the busy timeout is ErrBusy, and reads work once it lets go.
func TestAuditPageWhileAnotherProgramHoldsTheDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	require.NoError(t, s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Actor: "operator", Subject: "reader"}))
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
// runs is two exact seeks on one index, the rest of the cursor's microsecond
// and every older one, merged in page order: no scan and no sort, with no
// filter, a time range, each equality with or without one, several equalities
// (the most selective walks) and all five. Not parallel: it sets the clock.
func TestAuditFilterQueryPlans(t *testing.T) {
	s := openStore(t, t.TempDir(), Options{})
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	record := func(at time.Time, ev audit.Event) {
		defer utils.SetClock(utils.FixedClock(at))()
		require.NoError(t, s.Record(t.Context(), ev))
	}
	record(base, audit.Event{Action: audit.ActionPasswordChanged, Actor: "rare", Subject: "rare"})
	for i := range 100 {
		record(base.Add(time.Duration(i+1)*time.Minute), audit.Event{Action: audit.ActionKeyUsageReset, Actor: "bulk", Subject: fmt.Sprint("bulk-", i)})
	}
	last, second := base.Add(100*time.Minute), base.Add(time.Minute)
	rare := audit.Filter{Subject: "rare", Actor: "rare", Action: audit.ActionPasswordChanged}
	for _, tc := range []struct {
		index, equality string // the walked index and its equality, if any
		filter          audit.Filter
		newest          string // the first page of one event; only the unfiltered history has more
	}{
		{"audit_time", "", audit.Filter{}, "bulk-99"},
		{"audit_time", "", audit.Filter{Since: &last}, "bulk-99"},
		{"audit_time", "", audit.Filter{Until: &second}, "rare"},
		{"audit_time", "", audit.Filter{Since: &base, Until: &second}, "rare"},
		{"audit_actor", "actor", audit.Filter{Actor: "rare"}, "rare"},
		{"audit_subject", "subject", audit.Filter{Subject: "rare"}, "rare"},
		{"audit_action", "action", audit.Filter{Action: rare.Action}, "rare"},
		{"audit_actor", "actor", audit.Filter{Actor: "bulk", Since: new(base.Add(10 * time.Minute)), Until: new(base.Add(11 * time.Minute))}, "bulk-9"},
		{"audit_subject", "subject", audit.Filter{Subject: "rare", Since: &base, Until: &second}, "rare"},
		{"audit_action", "action", audit.Filter{Action: rare.Action, Since: &base, Until: &second}, "rare"},
		{"audit_actor", "actor", audit.Filter{Actor: "rare", Action: rare.Action}, "rare"},
		{"audit_subject", "subject", audit.Filter{Subject: "rare", Action: rare.Action}, "rare"},
		{"audit_subject", "subject", audit.Filter{Subject: "rare", Actor: "rare", Action: rare.Action, Since: &base, Until: &second}, "rare"},
	} {
		query, args := newAuditQuery(tc.filter).page(math.MaxInt64, math.MaxInt64, 1)
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
		equality, older := "", "time<?"
		if tc.equality != "" {
			equality = tc.equality + "=? AND "
		}
		if tc.filter.Since != nil {
			older = "time>? AND time<?"
		}
		walk := "SEARCH audit_events USING INDEX " + tc.index + " ("
		assert.Equal(t, []string{"MERGE (UNION ALL)", "LEFT", walk + equality + "time=? AND seq<?)", "RIGHT", walk + equality + older + ")"}, plan, "%+v", tc.filter)
		// The plan does not show a second upper bound kept as a filter; until joins the cursor instead.
		assert.Equal(t, 2, strings.Count(query, "<"), "%+v: one upper bound per seek: %s", tc.filter, query)
		events, next, err := s.AuditPage(t.Context(), tc.filter, "", 1)
		require.NoError(t, err)
		assert.Equal(t, []string{tc.newest}, subjects(events), "%+v", tc.filter)
		assert.Equal(t, tc.filter == (audit.Filter{}), next != "", "%+v", tc.filter)
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
