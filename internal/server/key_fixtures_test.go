package server

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stretchr/testify/require"
)

// loadTestKey adds one real hashed key to a live server through its store.
// The commit notifies the auth cache; requests still authenticate cold, and
// subsequent PATCHes still use the real mutation path.
func loadTestKey(t *testing.T, s *Server, id string, key keys.VirtualKey) string {
	t.Helper()
	if key.TeamID == "" {
		key.TeamID, key.TeamRole = personalTeamIDPrefix+id, string(teams.RoleOwner)
		_, err := s.services.Teams.CreatePersonalTeam(t.Context(), key.TeamID, key.Name, "admin")
		require.NoError(t, err)
	} else {
		require.NotNil(t, s.accessStore.Snapshot().Team(key.TeamID))
		if key.TeamRole == "" {
			key.TeamRole = string(teams.RoleMember)
		}
	}
	if key.Role == "" {
		key.Role = "user"
	}
	key.CreatedBy, key.UpdatedBy = "admin", "admin"
	return seedTestKey(t, s.accessStore, id, key)
}

// seedTestKey adds an incidental key without touching any other record.
func seedTestKey(t *testing.T, store access.Store, id string, key keys.VirtualKey) string {
	t.Helper()
	_, raw, err := store.CreateKey(t.Context(), id, key, audit.Event{})
	require.NoError(t, err)
	return raw
}

// newTestAccessStore opens an empty store without people in a private directory.
func newTestAccessStore(t *testing.T) access.Store {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, seedTestStore(dir))
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}
