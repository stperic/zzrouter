package accesstest

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

var auditBase = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func hour(i int) time.Time { return auditBase.Add(time.Duration(i) * time.Hour) }

// filtered pages through every event f selects, newest first, and counts the pages.
func filtered(t *testing.T, s access.Store, f audit.Filter, limit int) ([]audit.Event, int) {
	t.Helper()
	var events []audit.Event
	for cursor, pages := "", 1; ; pages++ {
		page, next, err := s.AuditPage(t.Context(), f, cursor, limit)
		require.NoError(t, err)
		require.NotNil(t, page, "a page is never nil")
		events = append(events, page...)
		if next == "" {
			return events, pages
		}
		require.Len(t, page, limit, "only the last page is short")
		cursor = next
	}
}

// selects is the filter's meaning, written out independently of any store.
func selects(f audit.Filter, ev audit.Event) bool {
	return (f.Actor == "" || ev.Actor == f.Actor) && (f.Subject == "" || ev.Subject == f.Subject) &&
		(f.Action == "" || ev.Action == f.Action) &&
		(f.Since == nil || !ev.Time.Before(*f.Since)) && (f.Until == nil || ev.Time.Before(*f.Until))
}

func times(events []audit.Event) []time.Time {
	out := make([]time.Time, len(events))
	for i, ev := range events {
		out[i] = ev.Time
	}
	return out
}

// recordAt records ev with the process clock at at.
func recordAt(t *testing.T, s access.Store, at time.Time, ev audit.Event) {
	defer utils.SetClock(utils.FixedClock(at))()
	s.Record(t.Context(), ev)
}

// testAuditTime: the store stamps each event from its clock, ignoring the
// caller's Time, and never earlier than the event before, so a clock stepped
// back records the previous time and filters keep commit order.
func testAuditTime(t *testing.T, h Harness) {
	s := h.Store
	recordAt(t, s, hour(2), audit.Event{Time: hour(9), Action: audit.ActionKeyUsageReset, Actor: "contract", Subject: "first"})
	recordAt(t, s, hour(1), audit.Event{Action: audit.ActionKeyUsageReset, Actor: "contract", Subject: "stepped back"})
	recordAt(t, s, hour(3).Add(time.Nanosecond), audit.Event{Action: audit.ActionKeyUsageReset, Actor: "contract", Subject: "truncated"})
	events := auditEvents(t, s)
	require.Len(t, events, 3)
	assert.Equal(t, []time.Time{hour(2), hour(2), hour(3)}, times(events), "the store's clock, never back, to the microsecond")
	got, _ := filtered(t, s, audit.Filter{Since: new(hour(2)), Until: new(hour(3))}, 1)
	assert.Equal(t, []string{"stepped back", "first"}, subjectsOf(got), "newest first in commit order")
}

// testAuditFilters pages a history of two actors, three subjects and two
// actions an hour apart, with two pairs sharing a time (the second of one pair
// recorded while the clock stood behind): each filter alone and all together
// return exactly their events in commit order, skipping and repeating none.
func testAuditFilters(t *testing.T, h Harness) {
	s := h.Store
	for i := range 12 {
		at := hour(i)
		switch i {
		case 7:
			at = hour(6)
		case 11:
			at = hour(-1)
		}
		recordAt(t, s, at, audit.Event{Actor: []string{"alice", "bob"}[i%2], Subject: []string{"eng", "ops", "sales"}[i%3],
			Action: []audit.Action{audit.ActionKeyCreated, audit.ActionTeamUpdated}[i/3%2], RequestID: fmt.Sprint("event-", i)})
	}
	recorded := auditEvents(t, s)
	require.Len(t, recorded, 12)
	assert.Equal(t, hour(6), recorded[7].Time)
	assert.Equal(t, hour(10), recorded[11].Time)
	slices.Reverse(recorded)
	for name, f := range map[string]audit.Filter{
		"actor":           {Actor: "alice"},
		"subject":         {Subject: "ops"},
		"action":          {Action: audit.ActionTeamUpdated},
		"since":           {Since: new(hour(6))},
		"until":           {Until: new(hour(6))},
		"since and until": {Since: new(hour(3)), Until: new(hour(10))},
		"all":             {Actor: "bob", Subject: "eng", Action: audit.ActionTeamUpdated, Since: new(hour(1)), Until: new(hour(11))},
	} {
		want := slices.DeleteFunc(slices.Clone(recorded), func(ev audit.Event) bool { return !selects(f, ev) })
		require.NotEmpty(t, want, name)
		require.Less(t, len(want), len(recorded), name)
		for _, limit := range []int{1, 2, 100} {
			got, pages := filtered(t, s, f, limit)
			assert.Equal(t, want, got, "%s, pages of %d", name, limit)
			assert.Equal(t, max(1, (len(want)+limit-1)/limit), pages, "%s, pages of %d", name, limit)
		}
	}
	got, _ := filtered(t, s, audit.Filter{Since: new(hour(4)), Until: new(hour(6))}, 100)
	assert.Equal(t, []time.Time{hour(5), hour(4)}, times(got), "since is inclusive, until exclusive")
	got, _ = filtered(t, s, audit.Filter{Since: new(hour(4).Add(time.Nanosecond)), Until: new(hour(6).Add(time.Nanosecond))}, 1)
	assert.Equal(t, []time.Time{hour(6), hour(6), hour(5)}, times(got), "a bound compares with the time the history returns")
	got, _ = filtered(t, s, audit.Filter{Since: new(hour(10))}, 1)
	assert.Equal(t, []string{"event-11", "event-10"}, requests(got), "events sharing a time page in commit order")
	for name, f := range map[string]audit.Filter{
		"no match": {Actor: "nobody"}, "empty range": {Since: new(hour(4)), Until: new(hour(4))},
		"after the newest": {Since: new(hour(11))}, "before the oldest": {Until: new(hour(0))},
	} {
		page, next, err := s.AuditPage(t.Context(), f, "", 10)
		require.NoError(t, err)
		assert.NotNil(t, page, name)
		assert.Empty(t, page, name)
		assert.Empty(t, next, name)
	}
}

func subjectsOf(events []audit.Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Subject
	}
	return out
}

func requests(events []audit.Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.RequestID
	}
	return out
}

// testAuditFilterValuesAreLiteral: SQL wildcards, quotes, comment markers and a
// NUL in a filter value match exactly the event that carries them.
func testAuditFilterValuesAreLiteral(t *testing.T, h Harness) {
	s := h.Store
	values := []string{"%", "a_c", "abc", "x' OR '1'='1", `say "hi"`, "nul\x00byte", "nul", `back\slash`, "semi;--"}
	for _, value := range values {
		s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Actor: value, Subject: value})
	}
	for _, value := range values {
		for _, f := range []audit.Filter{{Actor: value}, {Subject: value}} {
			got, _ := filtered(t, s, f, 100)
			require.Len(t, got, 1, "%q", value)
			assert.Equal(t, value, got[0].Actor)
		}
	}
	for _, pattern := range []string{"%%", "_", "a%", "' OR 1=1 --", "nul\x00", "*"} {
		got, _ := filtered(t, s, audit.Filter{Subject: pattern}, 100)
		assert.Empty(t, got, "%q", pattern)
	}
}

// testAuditCursorBoundToFilter: a cursor reads on only under the filter that
// issued it, even where two filters select the same events.
func testAuditCursorBoundToFilter(t *testing.T, h Harness) {
	s := h.Store
	for i := range 4 {
		s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Actor: []string{"alice", "bob"}[i%2], Subject: "eng"})
	}
	first := auditEvents(t, s)[0].Time
	filters := []audit.Filter{{}, {Actor: "alice"}, {Actor: "bob"}, {Subject: "eng"}, {Action: audit.ActionKeyUsageReset}, {Since: &first}, {Until: new(first.Add(time.Hour))}}
	readers := append(slices.Clone(filters), audit.Filter{Actor: "eng"}) // the value of another issuer's field
	for i, issuer := range filters {
		_, cursor, err := s.AuditPage(t.Context(), issuer, "", 1)
		require.NoError(t, err)
		require.NotEmpty(t, cursor, "%+v", issuer)
		for j, reader := range readers {
			_, _, err := s.AuditPage(t.Context(), reader, cursor, 1)
			if i == j {
				require.NoError(t, err, "%+v", issuer)
			} else {
				require.ErrorIs(t, err, audit.ErrCursor, "issued under %+v, read under %+v", issuer, reader)
			}
		}
	}
	_, cursor, err := s.AuditPage(t.Context(), audit.Filter{Since: &first}, "", 1)
	require.NoError(t, err)
	_, _, err = s.AuditPage(t.Context(), audit.Filter{Since: new(first.In(time.FixedZone("UTC-5", -5*3600)))}, cursor, 1)
	require.NoError(t, err, "the same instant in another zone is the same filter")
}
