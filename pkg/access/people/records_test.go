package people

import (
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
