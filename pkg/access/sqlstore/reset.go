package sqlstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
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

// ResetResult is what a reset's commit did; nothing stores it beyond its
// store.reset audit row.
type ResetResult struct {
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

// Reset removes what PreviewReset names, under the writer gate, once admit,
// which must not call the store, accepts the published meta. The removal and
// its store.reset audit row, naming backupID, commit in one transaction.
func (s *Store) Reset(ctx context.Context, backupID string, admit func(Meta) error) (ResetResult, error) {
	var result ResetResult
	var committed Meta
	err := s.mutate(ctx, keyRecords|teamRecords|peopleRecords, func(t *tx) error {
		if err := admit(t.meta); err != nil {
			return err
		}
		next, removed, err := resetRecords(t.records)
		if err != nil {
			return err
		}
		result = ResetResult{OperationID: uuid.NewString(), CreatedAt: utils.NowUTC(), BackupID: backupID, OldRevision: t.meta.Revision, Removed: removed}
		t.records, t.committed = next, &committed
		t.record(audit.Event{Action: audit.ActionStoreReset, Subject: t.meta.StoreID, Metadata: map[string]any{
			"operation_id": result.OperationID, "backup_id": backupID,
			"app_keys": removed.AppKeys, "teams": removed.Teams, "people": removed.People, "grants": removed.Grants,
		}})
		return nil
	})
	if err != nil {
		return ResetResult{}, err
	}
	result.StoreID, result.Generation, result.NewRevision, result.Sequence = committed.StoreID, committed.Generation, committed.Revision, committed.Sequence
	return result, nil
}
