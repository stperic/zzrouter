package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ErrReceiptConflict refuses a request whose idempotency key a stored receipt
// holds for another body.
var ErrReceiptConflict = errors.New("a different request body is recorded under this idempotency key")

const (
	receiptKindReset = "reset"
	receiptInsert    = "INSERT INTO maintenance_receipts (operation_id, kind, principal, idempotency_key, body_sha256, created_at, result) VALUES (?, ?, ?, ?, ?, ?, ?)"
	receiptSelect    = "SELECT body_sha256, result FROM maintenance_receipts WHERE kind = ? AND principal = ? AND idempotency_key = ?"
)

// ResetCounts are the records a reset removes, by kind.
type ResetCounts struct {
	AppKeys int `json:"app_keys"`
	Teams   int `json:"teams"`
	People  int `json:"people"`
	Grants  int `json:"grants"` // invitations, sign-in and recovery codes, used or not
}

// ResetPreview is what a reset of one published state removes and keeps.
type ResetPreview struct {
	Meta
	Removes ResetCounts
	Owner   *people.Person // nil: no owner, so nothing stays and owner setup stays open
	Team    *teams.Team    // the owner's team, kept with its policy and spend scope
}

// ResetRequest names a reset by the request that asks for it.
type ResetRequest struct {
	Principal, IdempotencyKey, BodySHA256, BackupID string
}

// Receipt is a committed reset, stored as its request is answered so a replay
// after a restart answers the same.
type Receipt struct {
	OperationID string      `json:"operation_id"`
	CreatedAt   time.Time   `json:"created_at"`
	BackupID    string      `json:"backup_id"`
	StoreID     string      `json:"store_id"`
	Generation  string      `json:"generation"`
	OldRevision uint64      `json:"old_revision"`
	NewRevision uint64      `json:"new_revision"`
	Sequence    uint64      `json:"sequence"`
	Removed     ResetCounts `json:"removed"`
}

// receiptRow is a receipt with the request it answers.
type receiptRow struct {
	kind, principal, key, bodySHA256 string
	Receipt
}

// values stamps the receipt with the commit's meta and returns its row.
func (r *receiptRow) values(oldRevision uint64, m Meta) ([]any, error) {
	r.StoreID, r.Generation, r.Sequence = m.StoreID, m.Generation, m.Sequence
	r.OldRevision, r.NewRevision = oldRevision, m.Revision
	result, err := json.Marshal(r.Receipt)
	return []any{r.OperationID, r.kind, r.principal, r.key, r.bodySHA256, r.CreatedAt.UnixMicro(), string(result)}, err
}

// resetRecords is the one reset rule: r with no app key, no team but the
// owner's and only what people.Reset keeps, and the counts it removes.
func resetRecords(r records) (records, ResetCounts, error) {
	before := r.people.Records()
	after := people.Reset(before)
	kept, err := people.NewState(after)
	if err != nil {
		return records{}, ResetCounts{}, err
	}
	next := records{keys: map[string]*keys.VirtualKey{}, teams: map[string]*teams.Team{}, people: kept}
	if owner := kept.Owner(); owner != nil {
		next.teams[owner.TeamID] = r.teams[owner.TeamID]
	}
	return next, ResetCounts{AppKeys: len(r.keys), Teams: len(r.teams) - len(next.teams),
		People: len(before.People) - len(after.People), Grants: len(before.Grants)}, nil
}

// PreviewReset applies the reset rule to the published state, without the
// writer gate. A failed store returns ErrFailed.
func (s *Store) PreviewReset() (ResetPreview, error) {
	published := s.pub.Load()
	if published.snap.Failed() {
		return ResetPreview{}, ErrFailed
	}
	next, removes, err := resetRecords(published.records)
	preview := ResetPreview{Meta: published.meta, Removes: removes, Owner: next.people.Owner()}
	if preview.Owner != nil {
		preview.Team = next.teams[preview.Owner.TeamID].Clone()
	}
	return preview, err
}

// Reset removes what PreviewReset names, under the writer gate. A receipt
// stored for r's principal and idempotency key answers first: the same body
// gets it back, another body ErrReceiptConflict. Otherwise admit, which must
// not call the store, judges the published meta; then the removal, its
// store.reset audit row and the receipt commit in one transaction.
func (s *Store) Reset(ctx context.Context, r ResetRequest, admit func(Meta) error) (Receipt, error) {
	row := &receiptRow{kind: receiptKindReset, principal: r.Principal, key: r.IdempotencyKey, bodySHA256: r.BodySHA256}
	var stored Receipt
	var found bool
	err := s.mutate(ctx, keyRecords|teamRecords|peopleRecords, func(t *tx) error {
		var err error
		if stored, found, err = s.receipt(ctx, row); err != nil || found {
			return err
		}
		if err := admit(t.meta); err != nil {
			return err
		}
		next, removed, err := resetRecords(t.records)
		if err != nil {
			return err
		}
		row.Receipt = Receipt{OperationID: uuid.NewString(), CreatedAt: utils.NowUTC(), BackupID: r.BackupID, Removed: removed}
		t.records, t.receipt = next, row
		t.record(audit.Event{Action: audit.ActionStoreReset, Subject: t.meta.StoreID, Metadata: map[string]any{
			"operation_id": row.OperationID, "backup_id": r.BackupID,
			"app_keys": removed.AppKeys, "teams": removed.Teams, "people": removed.People, "grants": removed.Grants,
		}})
		return nil
	})
	switch {
	case err != nil:
		return Receipt{}, err
	case found:
		return stored, nil
	}
	return row.Receipt, nil
}

// receipt reads the receipt stored for row's request: found for the same
// body, ErrReceiptConflict for another.
func (s *Store) receipt(ctx context.Context, row *receiptRow) (Receipt, bool, error) {
	var hash, result string
	var stored Receipt
	err := s.db.SQL().QueryRowContext(ctx, receiptSelect, row.kind, row.principal, row.key).Scan(&hash, &result)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return stored, false, nil
	case err != nil:
		return stored, false, busy(err)
	case hash != row.bodySHA256:
		return stored, false, ErrReceiptConflict
	}
	return stored, true, json.Unmarshal([]byte(result), &stored)
}
