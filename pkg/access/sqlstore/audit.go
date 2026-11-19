package sqlstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/stperic/zzrouter/pkg/audit"
)

const maxAuditPage = 100

var auditPage = auditTable.selectAll + " WHERE seq < ? ORDER BY seq DESC LIMIT ?"

// auditAppend numbers a row one past the last, so seq counts rows without
// gaps in the order commits list them; the commit sequence counts commits.
var auditAppend = "INSERT INTO audit_events (" + auditTable.columns + ") SELECT coalesce(max(seq), 0) + 1" +
	strings.Repeat(", ?", len(auditCols(&auditRow{}))-1) + " FROM audit_events"

// pageCursor is the position after a page. The generation names the stored
// content, so a cursor fails instead of skipping events once that is replaced.
type pageCursor struct {
	Generation string `json:"g"`
	Before     int64  `json:"b"`
}

// AuditPage returns up to limit events older than cursor, newest first, and the
// opaque cursor of the next page, empty after the oldest event. A cursor from
// another generation is audit.ErrCursor.
func (s *Store) AuditPage(ctx context.Context, cursor string, limit int) ([]audit.Event, string, error) {
	if limit < 1 || limit > maxAuditPage {
		return nil, "", fmt.Errorf("audit limit must be between 1 and %d", maxAuditPage)
	}
	generation := s.pub.Load().meta.generation
	position := pageCursor{Generation: generation, Before: math.MaxInt64}
	if cursor != "" {
		position = pageCursor{}
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(data, &position) != nil || position.Generation != generation {
			return nil, "", audit.ErrCursor
		}
	}
	var found []auditRow
	s.gate.Lock()
	err := s.refusal
	if err == nil {
		err = scanAll(ctx, s.db.SQL(), auditCols, func(r *auditRow) { found = append(found, *r) }, auditPage, position.Before, limit+1)
	}
	s.gate.Unlock()
	if err != nil {
		return nil, "", err
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
