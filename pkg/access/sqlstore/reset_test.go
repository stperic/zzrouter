package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/accesstest"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

func resetRequest(key string) ResetRequest {
	return ResetRequest{Principal: audit.ActorLocalOperator, IdempotencyKey: key, BodySHA256: strings.Repeat("a", 64), BackupID: "backup-1"}
}

func admitAll(Meta) error { return nil }

// populate gives s an owner whose workspace has a policy and a member key, a
// key with its personal team, another team with a member, a pending
// invitation, a recovery grant and onboarding progress.
func populate(t *testing.T, s *Store) *people.Person {
	t.Helper()
	owner := claim(t, s)
	_, err := s.UpdateTeam(t.Context(), "workspace", func(team *teams.Team) error {
		team.QuotaConfig = quota.QuotaConfig{SpendLimit: 7, ResetPeriod: "monthly"}
		team.AllowedModels = []string{"qwen"}
		return nil
	}, audit.Event{})
	require.NoError(t, err)
	_, _, err = s.CreateKey(t.Context(), "joined", keys.VirtualKey{Name: "Joined", Role: keys.RoleUser, TeamID: "workspace", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	_, _, err = s.CreateKey(t.Context(), "solo", readerKey(), soloEvent)
	require.NoError(t, err)
	_, err = s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
	require.NoError(t, err)
	invitation := bound(t, s, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "user"})
	member, err := s.People().AcceptInvitation(t.Context(), invitation.Token, testProof, people.CreatePerson{Username: "member", Name: "Member", Password: testPassword})
	require.NoError(t, err)
	bound(t, s, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "user"})
	_, err = s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeRecovery, TeamID: "eng", PersonID: member.ID, Issuer: installationIssuer})
	require.NoError(t, err)
	require.NoError(t, s.People().UpdateProgress(t.Context(), people.ProgressUpdate{Step: new("model")}))
	return owner
}

// TestResetRemovesWhatItsPreviewCounts: the preview's counts are exactly what
// the reset removes, and both keep the owner, under a new credential revision,
// and the owner's team with its policy; the commit names every removed key
// and team for the spend subscriber; one store.reset row joins an intact
// history, and the receipt carries the revisions and sequence it committed.
func TestResetRemovesWhatItsPreviewCounts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, contractOptions)
	owner := populate(t, s)
	var commits []access.Commit
	s.OnCommit(func(c access.Commit) { commits = append(commits, c) })
	before, history := s.Snapshot(), storedEvents(t, dir)
	revision, sequence := versions(s)

	preview, err := s.PreviewReset()
	require.NoError(t, err)
	assert.Equal(t, ResetCounts{AppKeys: 2, Teams: 2, People: 1, Grants: len(before.PeopleState().Records().Grants)}, preview.Removes)
	assert.GreaterOrEqual(t, preview.Removes.Grants, 2, "the pending invitation and the recovery grant at least")
	assert.Equal(t, s.Meta(), preview.Meta)
	assert.Equal(t, owner.ID, preview.Owner.ID)
	assert.Equal(t, before.Team("workspace"), preview.Team)

	ctx := audit.WithIdentity(t.Context(), audit.ActorLocalOperator, "reset-request")
	receipt, err := s.Reset(ctx, resetRequest("k1"), admitAll)
	require.NoError(t, err)
	assert.Equal(t, preview.Removes, receipt.Removed)
	after := s.Snapshot()
	assert.Empty(t, maps.Collect(after.Keys()))
	assert.Equal(t, map[string]*teams.Team{"workspace": before.Team("workspace")}, maps.Collect(after.Teams()), "the owner's team keeps its policy")
	require.Len(t, after.People(), 1)
	kept := after.Owner()
	assert.Equal(t, owner.ID, kept.ID)
	assert.NotEqual(t, before.Owner().Revision, kept.Revision, "earlier sessions stop resolving")
	_, err = s.People().Authenticate(t.Context(), "owner", testPassword)
	require.NoError(t, err, "the owner's password still works")
	assert.Empty(t, after.PeopleState().Records().Grants)
	assert.Equal(t, people.Progress{}, after.Progress())
	assert.True(t, after.Initialized())

	require.Len(t, commits, 1)
	slices.Sort(commits[0].RemovedKeys)
	slices.Sort(commits[0].RemovedTeams)
	assert.Equal(t, access.Commit{RemovedKeys: []string{"joined", "solo"}, RemovedTeams: []string{"eng", teams.PersonalID("solo")}}, commits[0])

	stored := storedEvents(t, dir)
	require.Len(t, stored, len(history)+1)
	assert.Equal(t, history, stored[:len(history)], "the history before the reset is intact")
	last := stored[len(history)]
	assert.Equal(t, [4]any{audit.ActionStoreReset, audit.ActorLocalOperator, s.Meta().StoreID, receipt.OperationID},
		[4]any{last.Action, last.Actor, last.Subject, last.Metadata["operation_id"]})
	assert.Equal(t, Receipt{OperationID: receipt.OperationID, CreatedAt: receipt.CreatedAt, BackupID: "backup-1", StoreID: s.Meta().StoreID,
		Generation: s.Meta().Generation, OldRevision: revision, NewRevision: revision + 1, Sequence: sequence + 1, Removed: preview.Removes}, receipt)
	assert.Equal(t, [2]uint64{revision + 1, sequence + 1}, [2]uint64{s.Meta().Revision, s.Meta().Sequence})
}

// TestResetWithoutOwnerRemovesEverything: with no owner claimed, a pending
// setup grant, its workspace, every key and team go, and owner setup stays open.
func TestResetWithoutOwnerRemovesEverything(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), contractOptions)
	_, err := s.IssueSetup(t.Context(), installationIssuer)
	require.NoError(t, err)
	_, _, err = s.CreateKey(t.Context(), "solo", readerKey(), soloEvent)
	require.NoError(t, err)
	_, err = s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
	require.NoError(t, err)

	preview, err := s.PreviewReset()
	require.NoError(t, err)
	assert.Nil(t, preview.Owner)
	assert.Nil(t, preview.Team)
	assert.Equal(t, ResetCounts{AppKeys: 1, Teams: 3, Grants: 1}, preview.Removes)
	receipt, err := s.Reset(t.Context(), resetRequest("k1"), admitAll)
	require.NoError(t, err)
	assert.Equal(t, preview.Removes, receipt.Removed)
	snap := s.Snapshot()
	assert.Empty(t, maps.Collect(snap.Teams()))
	assert.Empty(t, maps.Collect(snap.Keys()))
	assert.Empty(t, snap.PeopleState().Records().Grants)
	assert.False(t, snap.Initialized())
	grant, err := s.IssueSetup(t.Context(), installationIssuer)
	require.NoError(t, err, "owner setup stays open")
	_, err = s.People().ClaimOwner(t.Context(), bound(t, s, people.GrantRequest{Purpose: people.PurposeSetup, TeamID: grant.TeamID, Role: "admin"}).Token,
		testProof, people.CreatePerson{Username: "owner", Name: "Owner", Password: testPassword})
	require.NoError(t, err)
}

// TestResetReplaysItsReceipt: the receipt for a principal and idempotency key
// answers before admit, in this process and after a reopen, with no second
// mutation or audit row; another body under the key is ErrReceiptConflict.
func TestResetReplaysItsReceipt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, contractOptions)
	populate(t, s)
	first, err := s.Reset(t.Context(), resetRequest("k1"), admitAll)
	require.NoError(t, err)
	_, _, err = s.CreateKey(t.Context(), "after", readerKey(), audit.Event{})
	require.NoError(t, err)
	revision, sequence := versions(s)
	events := len(storedEvents(t, dir))
	refused := func(Meta) error { return errors.New("the token expired") }

	replay := func(s *Store) {
		t.Helper()
		got, err := s.Reset(t.Context(), resetRequest("k1"), refused)
		require.NoError(t, err, "the receipt answers before admit")
		assert.Equal(t, first, got)
		r, q := versions(s)
		assert.Equal(t, [2]uint64{revision, sequence}, [2]uint64{r, q}, "no second mutation")
		assert.NotNil(t, s.Snapshot().Key("after"))
		assert.Len(t, storedEvents(t, dir), events, "no second audit row")
	}
	replay(s)
	require.NoError(t, s.Close())
	s = openStore(t, dir, contractOptions)
	replay(s)

	other := resetRequest("k1")
	other.BodySHA256 = strings.Repeat("b", 64)
	_, err = s.Reset(t.Context(), other, admitAll)
	require.ErrorIs(t, err, ErrReceiptConflict)
	other = resetRequest("k1")
	other.Principal = "static:admin"
	_, err = s.Reset(t.Context(), other, admitAll)
	require.NoError(t, err, "a receipt answers its own principal only")
	assert.Nil(t, s.Snapshot().Key("after"))
}

// TestResetAdmitRefusalChangesNothing: admit sees the published meta, and
// its refusal returns unchanged with no record, audit row or receipt changed.
func TestResetAdmitRefusalChangesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, contractOptions)
	populate(t, s)
	before, events := s.Snapshot(), len(storedEvents(t, dir))
	refusal := errors.New("the revision changed")
	var seen Meta
	_, err := s.Reset(t.Context(), resetRequest("k1"), func(m Meta) error { seen = m; return refusal })
	require.ErrorIs(t, err, refusal)
	assert.Equal(t, s.Meta(), seen)
	assert.Same(t, before, s.Snapshot())
	assert.Len(t, storedEvents(t, dir), events)
	_, err = s.Reset(t.Context(), resetRequest("k1"), admitAll)
	require.NoError(t, err, "a refusal stores no receipt")
	assert.Empty(t, maps.Collect(s.Snapshot().Keys()))
}

// TestResetRacesKeyCreation: a key created around a reset is removed by it,
// with its personal team, or created after it, with its personal team; never
// half. The writer gate orders them, and both orders occur.
func TestResetRacesKeyCreation(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), contractOptions)
	claim(t, s)
	created := map[bool]int{}
	for round := 0; created[true] == 0 || created[false] == 0; round++ {
		require.Less(t, round, 12, "both orders occur")
		_, err := s.CreateTeam(t.Context(), "gate", engineering(), audit.Event{})
		require.NoError(t, err)
		id := fmt.Sprint("key-", round)
		create := func() {
			_, _, err := s.CreateKey(context.Background(), id, readerKey(), audit.Event{})
			assert.NoError(t, err)
		}
		reset := func() {
			_, err := s.Reset(context.Background(), resetRequest(id), admitAll)
			assert.NoError(t, err)
		}
		if round%2 == 0 {
			accesstest.QueuedRace(t, s, "gate", create, reset)
		} else {
			accesstest.QueuedRace(t, s, "gate", reset, create)
		}
		snap := s.Snapshot()
		after := snap.Key(id) != nil
		assert.Equal(t, after, snap.Team(teams.PersonalID(id)) != nil, "round %d: the key and its team move together", round)
		created[after]++
		if after {
			require.NoError(t, s.DeleteKey(t.Context(), id, audit.Event{}))
		}
	}
}
