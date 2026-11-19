package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/audit"
)

// auditAppend numbers a row one past the last, so seq counts rows without
// gaps in the order commits list them; the commit sequence counts commits.
var auditAppend = "INSERT INTO audit_events (" + auditTable.columns + ") SELECT coalesce(max(seq), 0) + 1" +
	strings.Repeat(", ?", len(auditCols(&auditRow{}))-1) + " FROM audit_events"

// pageCursor is the position after a page: the last event's stored time and
// seq. Pages run newest first by (time, seq), the commit order whenever the
// clock did not step back. The generation names the stored content and Filter
// the query, so a cursor fails instead of skipping events once either changes.
// Seq is never zero in a cursor the store issued.
type pageCursor struct {
	Generation string `json:"g"`
	Time       int64  `json:"t"`
	Seq        int64  `json:"s"`
	Filter     string `json:"f,omitempty"`
}

// auditQuery is a filter as SQL conditions over bound arguments; no value is
// ever part of the SQL text.
type auditQuery struct {
	terms []string
	args  []any
	until *int64 // exclusive, in stored micros
}

func (q *auditQuery) add(term string, arg any) {
	q.terms, q.args = append(q.terms, term), append(q.args, arg)
}

// newAuditQuery walks the index of the most selective equality set (a subject
// names one record, an actor one of a few, an action a kind); "+" keeps the
// planner off the others, which are checked on that index's rows. Every index
// ends in (time, seq), so a time range bounds the walk.
func newAuditQuery(f audit.Filter) auditQuery {
	var q auditQuery
	index := ""
	for _, eq := range []struct{ column, value string }{{"subject", f.Subject}, {"actor", f.Actor}, {"action", string(f.Action)}} {
		if eq.value != "" {
			q.add(index+eq.column+" = ?", eq.value)
			index = "+"
		}
	}
	if f.Since != nil {
		q.add("time >= ?", ceilMicros(*f.Since))
	}
	if f.Until != nil {
		q.until = new(ceilMicros(*f.Until))
	}
	return q
}

// ceilMicros is t in stored microseconds rounded up, so a stored time compares
// with it as with t.
func ceilMicros(t time.Time) int64 { return t.Add(time.Microsecond - 1).UnixMicro() }

// page reads up to limit+1 matching events below (t, s), newest first. Until
// joins the cursor as one upper bound, the nearer: time < until is below
// (until, MinInt64). SQLite seeks on a row value only up to a rowid column, so
// the bound splits into the rest of its microsecond and every older one, two
// exact seeks the merge keeps in page order without a sort.
func (q auditQuery) page(t, s int64, limit int) (string, []any) {
	if q.until != nil && *q.until <= t {
		t, s = *q.until, math.MinInt64
	}
	arm := func(bound string) string {
		return auditTable.selectAll + " WHERE " + strings.Join(append(slices.Clone(q.terms), bound), " AND ")
	}
	query := arm("time = ? AND seq < ?") + " UNION ALL " + arm("time < ?") + " ORDER BY time DESC, seq DESC LIMIT ?"
	return query, slices.Concat(q.args, []any{t, s}, q.args, []any{t, limit + 1})
}

// filterDigest binds a cursor to f's values as the query compares them; no
// filter has none.
func filterDigest(f audit.Filter) string {
	if f == (audit.Filter{}) {
		return ""
	}
	bound := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return fmt.Sprint(ceilMicros(*t))
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%q %q %q %q %q", f.Actor, f.Subject, f.Action, bound(f.Since), bound(f.Until)))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuditPage returns up to limit events f selects after cursor, newest first by
// time and then seq, and the opaque cursor of the next page, empty after the
// last. A cursor from another generation or filter is audit.ErrCursor.
func (s *Store) AuditPage(ctx context.Context, f audit.Filter, cursor string, limit int) ([]audit.Event, string, error) {
	if limit < 1 || limit > audit.MaxPage {
		return nil, "", fmt.Errorf("audit limit must be between 1 and %d", audit.MaxPage)
	}
	generation := s.pub.Load().meta.Generation
	position := pageCursor{Generation: generation, Time: math.MaxInt64, Seq: math.MaxInt64, Filter: filterDigest(f)}
	if cursor != "" {
		want := position
		position = pageCursor{}
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(data, &position) != nil || position.Seq == 0 ||
			position.Generation != want.Generation || position.Filter != want.Filter {
			return nil, "", audit.ErrCursor
		}
	}
	query, args := newAuditQuery(f).page(position.Time, position.Seq, limit)
	var found []auditRow
	s.gate.Lock()
	err := s.refusal
	if err == nil {
		err = scanAll(ctx, s.db.SQL(), auditCols, func(r *auditRow) { found = append(found, *r) }, query, args...)
	}
	s.gate.Unlock()
	if err != nil {
		return nil, "", busy(err)
	}
	next := ""
	if len(found) > limit {
		found = found[:limit]
		position.Time, position.Seq = found[limit-1].Time.UnixMicro(), found[limit-1].seq
		data, err := json.Marshal(position)
		if err != nil {
			return nil, "", err
		}
		next = base64.RawURLEncoding.EncodeToString(data)
	}
	events := make([]audit.Event, len(found))
	for i, r := range found {
		events[i] = r.Event
	}
	return events, next, nil
}
