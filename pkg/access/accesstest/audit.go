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
	seen := map[string]bool{}
	for cursor, pages := "", 1; ; pages++ {
		page, next, err := s.AuditPage(t.Context(), f, cursor, limit)
		require.NoError(t, err)
		require.NotNil(t, page, "a page is never nil")
		events = append(events, page...)
		if next == "" {
			return events, pages
		}
		require.Len(t, page, limit, "only the last page is short")
		require.False(t, seen[next], "a cursor came back: paging would never end")
		seen[next] = true
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
	require.NoError(t, s.Record(t.Context(), ev))
}

// ordered is events newest first by time, ties by recording order, the page
// order written out independently of any store; events are in recording order.
func ordered(events []audit.Event) []audit.Event {
	out := slices.Clone(events)
	slices.Reverse(out)
	slices.SortStableFunc(out, func(a, b audit.Event) int { return b.Time.Compare(a.Time) })
	return out
}

// pagesAgree pages f at several sizes and requires exactly want each time,
// no event skipped or repeated.
func pagesAgree(t *testing.T, s access.Store, f audit.Filter, want []audit.Event, name string) {
	t.Helper()
	for _, limit := range []int{1, 2, 100} {
		got, pages := filtered(t, s, f, limit)
		assert.Equal(t, want, got, "%s, pages of %d", name, limit)
		assert.Equal(t, max(1, (len(want)+limit-1)/limit), pages, "%s, pages of %d", name, limit)
	}
}

// testAuditTime: the store stamps each event with its clock's time and
// nothing else, ignoring the caller's Time. An event recorded while the clock
// ran a year ahead keeps that time and later events keep theirs; one recorded
// after the clock stepped back sorts by its own, earlier time.
func testAuditTime(t *testing.T, h Harness) {
	s := h.Store
	ahead := hour(0).AddDate(1, 0, 0)
	var want []audit.Event
	for _, step := range []struct {
		at      time.Time
		subject string
	}{{ahead, "ahead"}, {hour(1), "a"}, {hour(2), "b"}, {hour(3).Add(time.Nanosecond), "c"}, {hour(2).Add(30 * time.Minute), "stepped back"}} {
		ev := audit.Event{Action: audit.ActionKeyUsageReset, Actor: "contract", Subject: step.subject}
		recordAt(t, s, step.at, audit.Event{Time: hour(-9), Action: ev.Action, Actor: ev.Actor, Subject: ev.Subject})
		ev.Time = step.at.Truncate(time.Microsecond)
		want = append(want, ev)
	}
	want = ordered(want)
	assert.Equal(t, []string{"ahead", "c", "stepped back", "b", "a"}, subjectsOf(want))
	pagesAgree(t, s, audit.Filter{}, want, "everything, the event ahead first")
	pagesAgree(t, s, audit.Filter{Until: new(hour(4))}, want[1:], "until now leaves out the event ahead")
	pagesAgree(t, s, audit.Filter{Since: new(hour(2)), Until: new(hour(3))}, want[2:4], "the stepped-back event by its own time")
}

// testAuditTies: events stamped in one microsecond page in recording order,
// across page boundaries, skipping and repeating none.
func testAuditTies(t *testing.T, h Harness) {
	s := h.Store
	var want []audit.Event
	for i := range 7 {
		ev := audit.Event{Time: hour(5), Action: audit.ActionKeyUsageReset, Actor: "contract", Subject: fmt.Sprint("tie-", i)}
		recordAt(t, s, hour(5), ev)
		want = append(want, ev)
	}
	recordAt(t, s, hour(4), audit.Event{Action: audit.ActionKeyUsageReset, Actor: "contract", Subject: "older"})
	want = ordered(want)
	assert.Equal(t, "tie-6", want[0].Subject)
	pagesAgree(t, s, audit.Filter{Since: new(hour(5))}, want, "one microsecond")
	pagesAgree(t, s, audit.Filter{Actor: "contract", Until: new(hour(5).Add(time.Microsecond))}, append(slices.Clone(want), audit.Event{Time: hour(4),
		Action: audit.ActionKeyUsageReset, Actor: "contract", Subject: "older"}), "into the next microsecond")
}

// testAuditFilters pages a history of two actors, three subjects and two
// actions an hour apart, with one event recorded while the clock ran ahead,
// one sharing another's time and one after the clock stepped back: each
// filter alone and all together return exactly their events in page order.
func testAuditFilters(t *testing.T, h Harness) {
	s := h.Store
	var recorded []audit.Event
	for i := range 12 {
		at := hour(i)
		switch i {
		case 3:
			at = hour(30)
		case 7:
			at = hour(6)
		case 11:
			at = hour(-1)
		}
		ev := audit.Event{Time: at, Actor: []string{"alice", "bob"}[i%2], Subject: []string{"eng", "ops", "sales"}[i%3],
			Action: []audit.Action{audit.ActionKeyCreated, audit.ActionTeamUpdated}[i/3%2], RequestID: fmt.Sprint("event-", i)}
		recordAt(t, s, at, ev)
		recorded = append(recorded, ev)
	}
	all := ordered(recorded)
	assert.Equal(t, []string{"event-3", "event-10", "event-9", "event-8", "event-7", "event-6"}, requests(all[:6]))
	pagesAgree(t, s, audit.Filter{}, all, "no filter")
	for name, f := range map[string]audit.Filter{
		"actor":           {Actor: "alice"},
		"subject":         {Subject: "ops"},
		"action":          {Action: audit.ActionTeamUpdated},
		"since":           {Since: new(hour(6))},
		"until":           {Until: new(hour(6))},
		"since and until": {Since: new(hour(3)), Until: new(hour(10))},
		"all":             {Actor: "bob", Subject: "eng", Action: audit.ActionTeamUpdated, Since: new(hour(1)), Until: new(hour(31))},
	} {
		want := slices.DeleteFunc(slices.Clone(all), func(ev audit.Event) bool { return !selects(f, ev) })
		require.NotEmpty(t, want, name)
		require.Less(t, len(want), len(all), name)
		pagesAgree(t, s, f, want, name)
	}
	got, _ := filtered(t, s, audit.Filter{Since: new(hour(4)), Until: new(hour(6))}, 100)
	assert.Equal(t, []time.Time{hour(5), hour(4)}, times(got), "since is inclusive, until exclusive")
	got, _ = filtered(t, s, audit.Filter{Since: new(hour(4).Add(time.Nanosecond)), Until: new(hour(6).Add(time.Nanosecond))}, 1)
	assert.Equal(t, []string{"event-7", "event-6", "event-5"}, requests(got), "a bound compares with the time the history returns")
	for name, f := range map[string]audit.Filter{
		"no match": {Actor: "nobody"}, "empty range": {Since: new(hour(4)), Until: new(hour(4))},
		"after the newest": {Since: new(hour(31))}, "before the oldest": {Until: new(hour(-1))},
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
		require.NoError(t, s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Actor: value, Subject: value}))
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
		require.NoError(t, s.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Actor: []string{"alice", "bob"}[i%2], Subject: "eng"}))
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
