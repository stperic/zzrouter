package sqlstore

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

// roundTrip writes in through its columns into an empty table and scans the one row back.
func roundTrip[R any](t *testing.T, db *sql.DB, tb table, cols func(*R) []col, in *R) *R {
	t.Helper()
	_, err := db.ExecContext(t.Context(), tb.upsert, ptrs(cols(in))...)
	require.NoError(t, err)
	var out []*R
	require.NoError(t, scanAll(t.Context(), db, cols, func(r *R) { out = append(out, r) }, tb.selectAll))
	require.Len(t, out, 1)
	return out[0]
}

func emptyDB(t *testing.T) *sql.DB {
	t.Helper()
	return openStore(t, t.TempDir(), Options{}).db.SQL()
}

// at is an instant with nanoseconds the store truncates to microseconds.
func at(year int) time.Time { return time.Date(year, 3, 4, 5, 6, 7, 123456789, time.UTC) }

func micro(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }

func nullColumn(t *testing.T, db *sql.DB, query string) bool {
	t.Helper()
	var value sql.NullString
	require.NoError(t, db.QueryRowContext(t.Context(), query).Scan(&value))
	return !value.Valid
}

func TestKeyCodecRoundTrip(t *testing.T) {
	t.Parallel()
	suspended, expires := at(2024), at(9999).In(time.FixedZone("ahead", 3600))
	full := keys.VirtualKey{
		ID: "reader", UUID: "uuid", Name: "Reader", Description: "agent: research", HashedKey: "hash",
		TeamID: "eng", TeamRole: "owner", Role: "admin", Suspended: true, SuspendedAt: &suspended, SuspendedBy: "ops",
		QuotaConfig: quota.QuotaConfig{RPMLimit: 1, TPMLimit: 2, MaxParallelRequests: 3, SpendLimit: 4.25, ResetPeriod: "daily", DefaultMaxTokens: 5},
		ExpiresAt:   &expires, CreatedAt: at(2025), UpdatedAt: at(2026), LastSeenAt: at(2027), CreatedBy: "creator", UpdatedBy: "updater",
		Metadata: map[string]string{"env": "ci"},
	}
	want := full.Clone()
	want.SuspendedAt, want.ExpiresAt = new(micro(suspended)), new(micro(expires))
	want.CreatedAt, want.UpdatedAt, want.LastSeenAt = micro(full.CreatedAt), micro(full.UpdatedAt), micro(full.LastSeenAt)
	assert.Equal(t, want, roundTrip(t, emptyDB(t), keyTable, keyCols, full.Clone()))

	bare := keys.VirtualKey{ID: "bare", Name: "Bare", HashedKey: "hash", TeamID: "eng", TeamRole: "member"}
	db := emptyDB(t)
	assert.Equal(t, &bare, roundTrip(t, db, keyTable, keyCols, bare.Clone()), "nil times and metadata read back as nil, zero times as zero")
	assert.True(t, nullColumn(t, db, "SELECT metadata FROM keys"))
	assert.True(t, nullColumn(t, db, "SELECT expires_at FROM keys"))

	empty := bare.Clone()
	empty.Metadata = map[string]string{}
	assert.Equal(t, empty, roundTrip(t, emptyDB(t), keyTable, keyCols, empty.Clone()), "empty metadata stays empty, not nil")
}

func TestTeamCodecRoundTrip(t *testing.T) {
	t.Parallel()
	suspended := at(2024)
	full := teams.Team{
		ID: "eng", UUID: "uuid", Name: "Engineering", Kind: teams.KindShared, AllowedModels: []string{"qwen", "llama"},
		RouteAccess:    teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}},
		ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama"}},
		Suspended:      true, SuspendedAt: &suspended, SuspendedBy: "ops",
		QuotaConfig: quota.QuotaConfig{RPMLimit: 1, TPMLimit: 2, MaxParallelRequests: 3, SpendLimit: 4.5, ResetPeriod: "weekly", DefaultMaxTokens: 5},
		CreatedAt:   at(2025), UpdatedAt: at(2026), CreatedBy: "creator", UpdatedBy: "updater", Metadata: map[string]string{"dept": "r&d"},
	}
	want := full.Clone()
	want.SuspendedAt, want.CreatedAt, want.UpdatedAt = new(micro(suspended)), micro(full.CreatedAt), micro(full.UpdatedAt)
	assert.Equal(t, want, roundTrip(t, emptyDB(t), teamTable, teamCols, full.Clone()))

	// A zero policy marshals as its explicit default; NULL keeps it zero.
	zero := teams.Team{ID: "open", Kind: teams.KindPersonal}
	db := emptyDB(t)
	assert.Equal(t, &zero, roundTrip(t, db, teamTable, teamCols, zero.Clone()))
	for _, column := range []string{"allowed_models", "route_access", "provider_access", "metadata", "suspended_at"} {
		assert.True(t, nullColumn(t, db, "SELECT "+column+" FROM teams"), column)
	}

	open := teams.Team{ID: "none", Kind: teams.KindShared, AllowedModels: []string{}, RouteAccess: teams.RouteAccess{Mode: "none"}, ProviderAccess: teams.ProviderAccess{Mode: "all"}}
	assert.Equal(t, &open, roundTrip(t, emptyDB(t), teamTable, teamCols, open.Clone()), "an empty model list and explicit modes survive")
}

// Same-typed neighbouring columns hold different values, so a swapped
// column pair cannot read back equal.
func TestPeopleCodecRoundTrip(t *testing.T) {
	t.Parallel()
	for name, flags := range map[string][2]bool{"owner": {true, false}, "suspended": {false, true}} {
		person := people.PersonRecord{ //nolint:gosec // a fixture hash, not a credential
			Person:       people.Person{ID: "p1", Username: "alice", Name: "Alice", Role: "admin", TeamID: "eng", Owner: flags[0], Suspended: flags[1], Revision: "rev", CreatedAt: at(2025), UpdatedAt: at(2026)},
			PasswordHash: "$argon2id$v=19$m=65536,t=3,p=1$salt$hash",
		}
		want := person
		want.CreatedAt, want.UpdatedAt = micro(person.CreatedAt), micro(person.UpdatedAt)
		assert.Equal(t, &want, roundTrip(t, emptyDB(t), personTable, personCols, &person), name)
	}

	db := emptyDB(t)
	owner := people.PersonRecord{Person: people.Person{ID: "p1", Username: "alice", Name: "Alice", Role: "admin", TeamID: "eng", Revision: "rev"}}
	_, err := db.ExecContext(t.Context(), personTable.upsert, ptrs(personCols(&owner))...)
	require.NoError(t, err)
	for name, progress := range map[string]people.Progress{
		"dismissed":         {Step: "app", Dismissed: true, UpdatedAt: at(2026)},
		"response verified": {Step: "model", ResponseVerified: true, UpdatedAt: at(2027)},
		"app connected":     {Step: "test", AppConnected: true, UpdatedAt: at(2028)},
	} {
		got := roundTrip(t, db, installationTable, installationCols, &people.Records{OwnerID: "p1", Progress: progress})
		assert.Equal(t, "p1", got.OwnerID, name)
		progress.UpdatedAt = micro(progress.UpdatedAt)
		assert.Equal(t, progress, got.Progress, name)
	}
	got := roundTrip(t, db, installationTable, installationCols, &people.Records{})
	assert.Empty(t, got.OwnerID, "no owner reads back as none")
	assert.Equal(t, people.Progress{}, got.Progress, "zero progress, zero time included, reads back as zero")
	assert.True(t, nullColumn(t, db, "SELECT owner_id FROM installation"))

	used, revoked := at(2027), at(2028)
	issuer := people.Issuer{PersonID: "p2", Revision: "rev2", Reference: "installation"}
	for _, purpose := range []people.Purpose{people.PurposeSetup, people.PurposeSession, people.PurposeInvitation, people.PurposeRecovery} {
		for state, times := range map[string][2]*time.Time{"pending": {}, "used": {&used, nil}, "revoked": {nil, &revoked}} {
			grant := grantRow{hash: strings.Repeat("a", 64), GrantRecord: people.GrantRecord{
				Grant:  people.Grant{ID: "g", Purpose: purpose, TeamID: "eng", Role: "user", PersonID: "p1", CreatedAt: at(2025), ExpiresAt: at(2026), UsedAt: times[0], RevokedAt: times[1]},
				Issuer: issuer, BrowserHash: "browser",
			}}
			want := grant
			want.CreatedAt, want.ExpiresAt = micro(grant.CreatedAt), micro(grant.ExpiresAt)
			if times[0] != nil {
				want.UsedAt = new(micro(used))
			}
			if times[1] != nil {
				want.RevokedAt = new(micro(revoked))
			}
			assert.Equal(t, &want, roundTrip(t, emptyDB(t), grantTable, grantCols, &grant), "%s %s", purpose, state)
		}
	}
}

func TestAuditCodecRoundTrip(t *testing.T) {
	t.Parallel()
	row := auditRow{seq: 7, Event: audit.Event{Time: at(2026), Action: audit.ActionKeyCreated, Actor: "a", Subject: "s", RequestID: "r", Metadata: map[string]any{"n": 1.5, "s": "v"}}}
	want := row
	want.Time = micro(row.Time)
	assert.Equal(t, &want, roundTrip(t, emptyDB(t), auditTable, auditCols, &row))
}

// TestHostileStringsRoundTrip stores each string byte for byte or refuses it
// with the record's ErrInvalid; nothing is stored altered.
func TestHostileStringsRoundTrip(t *testing.T) {
	t.Parallel()
	hostile := map[string]string{
		"merge key": "<<", "tilde": "~", "null": "null", "infinity": ".inf", "document": "---", "nul": "a\x00b",
		"line separator": "a\u2028b", "bom": "\ufeffbom", "quotes": `'";--`, "like": "%_", "large": strings.Repeat("x", 1<<20),
	}
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	for name, value := range hostile {
		k := keys.VirtualKey{Name: value, TeamID: value, TeamRole: "member", Description: value, Metadata: map[string]string{value: value}}
		_, _, err := s.CreateKey(t.Context(), value, k, audit.Event{})
		require.NoError(t, err, name)
		_, err = s.CreateTeam(t.Context(), value, teams.Team{Name: value, Kind: teams.KindShared, AllowedModels: []string{value}, Metadata: map[string]string{value: value}}, audit.Event{})
		require.NoError(t, err, name)
	}
	// SQLite keeps TEXT bytes as given; JSON would replace invalid UTF-8.
	invalid := "bad\xff\xfe"
	_, _, err := s.CreateKey(t.Context(), invalid, keys.VirtualKey{Name: invalid, TeamID: invalid, TeamRole: "member"}, audit.Event{})
	require.NoError(t, err, "invalid UTF-8 in a TEXT column is stored as is")
	_, _, err = s.CreateKey(t.Context(), "meta", keys.VirtualKey{Name: "meta", TeamID: "t", TeamRole: "member", Metadata: map[string]string{"k": invalid}}, audit.Event{})
	require.ErrorIs(t, err, keys.ErrInvalid, "invalid UTF-8 in JSON metadata is refused")
	_, err = s.CreateTeam(t.Context(), "models", teams.Team{Kind: teams.KindShared, AllowedModels: []string{invalid}}, audit.Event{})
	require.ErrorIs(t, err, teams.ErrInvalid, "invalid UTF-8 in a JSON model list is refused")
	before := s.Snapshot()
	require.NoError(t, s.Close())

	snap := openStore(t, dir, Options{}).Snapshot()
	for name, value := range hostile {
		key, team := snap.Key(value), snap.Team(value)
		require.NotNil(t, key, name)
		require.NotNil(t, team, name)
		assert.Equal(t, microKey(before.Key(value)), key, name)
		assert.Equal(t, microTeam(before.Team(value)), team, name)
		assert.Equal(t, value, key.Metadata[value], name)
	}
	require.NotNil(t, snap.Key(invalid))
	assert.Equal(t, invalid, snap.Key(invalid).Name)
	assert.Nil(t, snap.Key("meta"))
	assert.Nil(t, snap.Team("models"))
}

// microKey and microTeam truncate the times a store keeps to the microsecond.
func microKey(k *keys.VirtualKey) *keys.VirtualKey {
	k.CreatedAt, k.UpdatedAt = micro(k.CreatedAt), micro(k.UpdatedAt)
	return k
}

func microTeam(team *teams.Team) *teams.Team {
	team.CreatedAt, team.UpdatedAt = micro(team.CreatedAt), micro(team.UpdatedAt)
	return team
}
