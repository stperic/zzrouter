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

// loadTestKey adds one real hashed key to a live server through its store,
// with its personal team when it names none. Requests still authenticate
// cold, and subsequent PATCHes still use the real mutation path.
func loadTestKey(t *testing.T, s *Server, id string, key keys.VirtualKey) string {
	t.Helper()
	if key.TeamID != "" && key.TeamRole == "" {
		key.TeamRole = string(teams.RoleMember)
	}
	if key.Role == "" {
		key.Role = keys.RoleUser
	}
	key.CreatedBy, key.UpdatedBy = "admin", "admin"
	return seedTestKey(t, s.accessStore, id, key)
}

// seedTestTeam adds an empty shared team for keys to join.
func seedTestTeam(t *testing.T, store access.Store, id string) {
	t.Helper()
	_, err := store.CreateTeam(t.Context(), id, teams.Team{Name: id, Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
}

// seedTestKey adds an incidental key; one that names no team gets its personal team.
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
