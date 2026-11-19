package people

import (
	"maps"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordsRoundTrip moves a state through its transfer form and back, and
// shows that neither side can edit the other afterwards.
func TestRecordsRoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	owner := claimTestOwner(t, s)
	invited, err := s.Issue(t.Context(), GrantRequest{Purpose: PurposeInvitation, TeamID: "engineering", Role: "user", Issuer: Issuer{PersonID: owner.ID, Revision: owner.Revision, Reference: "owner"}})
	require.NoError(t, err)
	require.NoError(t, s.RevokeGrant(t.Context(), invited.ID))
	published := s.state()

	records := published.Records()
	require.True(t, records.Initialized)
	require.Equal(t, owner.ID, records.OwnerID)
	require.Len(t, records.People, 1)
	assert.True(t, validPasswordHash(records.People[owner.ID].PasswordHash), "the transfer form carries the password hash")
	var revoked GrantRecord
	for _, grant := range records.Grants {
		if grant.ID == invited.ID {
			revoked = grant
		}
	}
	require.NotNil(t, revoked.RevokedAt)
	assert.Equal(t, owner.ID, revoked.Issuer.PersonID, "the transfer form carries the issuer")

	rebuilt, err := NewState(records)
	require.NoError(t, err)
	assert.Equal(t, published, rebuilt)

	// Editing the transfer form, or what built a state, changes no state.
	*revoked.RevokedAt = time.Time{}
	records.People[owner.ID] = PersonRecord{}
	records.OwnerID = "edited"
	assert.Equal(t, published.Records(), rebuilt.Records())
	assert.False(t, published.Invitations()[0].RevokedAt.IsZero())
	assert.False(t, rebuilt.Invitations()[0].RevokedAt.IsZero())

	empty, err := NewState(Records{})
	require.NoError(t, err, "no maps reads as none")
	assert.Empty(t, empty.People())
	assert.Equal(t, Records{People: map[string]PersonRecord{}, Grants: map[string]GrantRecord{}}, (*State)(nil).Records())
}

func TestNewStateValidates(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	claimTestOwner(t, s)
	for name, edit := range map[string]func(*Records){
		"initialized without an owner": func(r *Records) { r.OwnerID = "" },
		"owner not among the people":   func(r *Records) { r.OwnerID = "nobody" },
		"bad password hash": func(r *Records) {
			for id, p := range r.People {
				p.PasswordHash = "plain"
				r.People[id] = p
			}
		},
		"short grant digest": func(r *Records) {
			r.Grants["short"] = GrantRecord{Grant: Grant{ID: "g", Purpose: PurposeSession, ExpiresAt: utils.NowUTC()}, Issuer: Issuer{Reference: "x"}}
		},
	} {
		records := s.state().Records()
		edit(&records)
		_, err := NewState(records)
		require.Error(t, err, name)
	}
}

// TestRestaged gives every person a new revision and expires exactly the
// pending grants, leaving everything else, and its argument, as they were.
func TestRestaged(t *testing.T) {
	t.Parallel()
	now := utils.NowUTC()
	used, revoked := now.Add(-time.Hour), now.Add(-time.Minute)
	grant := func(expires time.Time, usedAt, revokedAt *time.Time) GrantRecord {
		return GrantRecord{Grant: Grant{ID: "grant", Purpose: PurposeSession, ExpiresAt: expires, UsedAt: usedAt, RevokedAt: revokedAt}, Issuer: Issuer{Reference: "installation"}, BrowserHash: "browser"}
	}
	r := Records{
		Initialized: true,
		OwnerID:     "owner",
		People: map[string]PersonRecord{
			"owner":  {Person: Person{ID: "owner", Username: "owner", Revision: "owner revision"}, PasswordHash: "owner hash"},
			"member": {Person: Person{ID: "member", Username: "member", Revision: "member revision"}, PasswordHash: "member hash"},
		},
		Grants: map[string]GrantRecord{
			"pending": grant(now.Add(time.Hour), nil, nil),
			"used":    grant(now.Add(time.Hour), &used, nil),
			"revoked": grant(now.Add(time.Hour), nil, &revoked),
			"expired": grant(now.Add(-time.Second), nil, nil),
		},
	}
	original := Records{Initialized: r.Initialized, OwnerID: r.OwnerID, People: maps.Clone(r.People), Grants: maps.Clone(r.Grants)}

	restaged := Restaged(r)
	assert.Equal(t, original, r, "the argument is unchanged")
	require.Len(t, restaged.People, 2)
	for id, p := range restaged.People {
		assert.NotEqual(t, r.People[id].Revision, p.Revision, id)
		assert.NotEmpty(t, p.Revision, id)
		p.Revision = r.People[id].Revision
		assert.Equal(t, r.People[id], p, "%s: only the revision changes", id)
	}
	assert.NotEqual(t, restaged.People["owner"].Revision, restaged.People["member"].Revision)
	pending := restaged.Grants["pending"]
	assert.False(t, active(pending.Grant), "a pending grant expires")
	assert.False(t, pending.ExpiresAt.Before(now), "at the time of the restore")
	pending.ExpiresAt = r.Grants["pending"].ExpiresAt
	assert.Equal(t, r.Grants["pending"], pending, "only its expiry changes")
	for _, hash := range []string{"used", "revoked", "expired"} {
		assert.Equal(t, r.Grants[hash], restaged.Grants[hash], hash)
	}
	assert.Equal(t, r.OwnerID, restaged.OwnerID)
}
