package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/audit"
)

// auditAppend numbers a row one past the last, so seq counts rows without
// gaps in the order commits list them; the commit sequence counts commits.
// Its time, the first value, is never earlier than the newest row's, so time
// follows seq and a time range is a seq range.
var auditAppend = "INSERT INTO audit_events (" + auditTable.columns + ") VALUES ((SELECT ifnull(max(seq), 0) + 1 FROM audit_events), " +
	"max(?1, ifnull((SELECT max(time) FROM audit_events), ?1))" + strings.Repeat(", ?", len(auditCols(&auditRow{}))-2) + ")"

// firstAt is the oldest seq at or after a time, one seek on audit_time, or
// the int64 limit when no event is that recent.
const firstAt = "ifnull((SELECT seq FROM audit_events WHERE time >= ? ORDER BY time, seq LIMIT 1), 9223372036854775807)"

// pageCursor is the position after a page. The generation names the stored
// content and Filter the query, so a cursor fails instead of skipping events
// once either changes.
type pageCursor struct {
	Generation string `json:"g"`
	Before     int64  `json:"b"`
	Filter     string `json:"f,omitempty"`
}

// auditQuery is a filter as SQL conditions over bound arguments; no value is
// ever part of the SQL text.
type auditQuery struct {
	terms []string
	args  []any
	until *int64 // stored micros
}

func (q *auditQuery) add(term string, arg any) {
	q.terms, q.args = append(q.terms, term), append(q.args, arg)
}

// newAuditQuery walks the index of the most selective equality set (a subject
// names one record, an actor one of a few, an action a kind); "+" keeps the
// planner off the others, which are checked on that index's rows. Time bounds
// the walk to a seq range; page joins until to the cursor.
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
		q.add("seq >= "+firstAt, ceilMicros(*f.Since))
	}
	if f.Until != nil {
		q.until = new(ceilMicros(*f.Until))
	}
	return q
}

// ceilMicros is t in stored microseconds rounded up, so a stored time compares
// with it as with t.
func ceilMicros(t time.Time) int64 { return t.Add(time.Microsecond - 1).UnixMicro() }

// page reads up to limit+1 matching events older than before, newest first.
// The cursor and until make one upper bound, so the walk seeks to the nearer;
// with two, the planner may seek to the cursor and step through every row down
// to until.
func (q auditQuery) page(before int64, limit int) (string, []any) {
	upper, args := "seq < ?", []any{before}
	if q.until != nil {
		upper, args = "seq < min(?, "+firstAt+")", append(args, *q.until)
	}
	where := strings.Join(append([]string{upper}, q.terms...), " AND ")
	return auditTable.selectAll + " WHERE " + where + " ORDER BY seq DESC LIMIT ?", append(append(args, q.args...), limit+1)
}

// filterDigest binds a cursor to f's values as the query compares them; no
// filter has none, so an unfiltered cursor keeps its form.
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

// AuditPage returns up to limit events f selects older than cursor, newest
// first, and the opaque cursor of the next page, empty after the oldest. A
// cursor from another generation or filter is audit.ErrCursor.
func (s *Store) AuditPage(ctx context.Context, f audit.Filter, cursor string, limit int) ([]audit.Event, string, error) {
	if limit < 1 || limit > audit.MaxPage {
		return nil, "", fmt.Errorf("audit limit must be between 1 and %d", audit.MaxPage)
	}
	generation := s.pub.Load().meta.generation
	position := pageCursor{Generation: generation, Before: math.MaxInt64, Filter: filterDigest(f)}
	if cursor != "" {
		want := position
		position = pageCursor{}
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(data, &position) != nil || position.Generation != want.Generation || position.Filter != want.Filter {
			return nil, "", audit.ErrCursor
		}
	}
	query, args := newAuditQuery(f).page(position.Before, limit)
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
		position.Before = found[limit-1].seq
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
