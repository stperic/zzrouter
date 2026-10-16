package server

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type testKeyIdentity struct {
	once      sync.Once
	raw, hash string
	err       error
}

var testKeyIdentities sync.Map

// prehashedTestIdentity amortizes incidental provisioning, never verification.
// Different IDs keep distinct credentials even when their stores are private.
func prehashedTestIdentity(t *testing.T, id string) (string, string) {
	t.Helper()
	value, _ := testKeyIdentities.LoadOrStore(id, &testKeyIdentity{})
	identity := value.(*testKeyIdentity)
	identity.once.Do(func() {
		identity.raw, identity.err = keys.GenerateRawKey()
		if identity.err == nil {
			identity.hash, identity.err = keys.HashKey(identity.raw)
		}
	})
	require.NoError(t, identity.err)
	return identity.raw, identity.hash
}

// loadTestKey replaces the incidental key population with one real hashed key.
// Load notifies the existing auth/index listeners; requests still authenticate
// cold, and subsequent PATCHes still use the real mutation path.
func loadTestKey(t *testing.T, s *Server, id string, key keys.VirtualKey) string {
	t.Helper()
	if key.TeamID == "" {
		key.TeamID, key.TeamRole = personalTeamIDPrefix+id, string(teams.RoleOwner)
		_, err := s.services.Teams.CreatePersonalTeam(key.TeamID, key.Name, "admin")
		require.NoError(t, err)
	} else {
		require.NotNil(t, s.teamStore.Get(key.TeamID))
		if key.TeamRole == "" {
			key.TeamRole = string(teams.RoleMember)
		}
	}
	if key.Role == "" {
		key.Role = "user"
	}
	key.CreatedBy, key.UpdatedBy = "admin", "admin"
	rawKey, record := prehashedTestRecord(t, id, key)
	path := filepath.Join(pkgConfig.NewConfigManager("zzrouter").GetNodeConfigDir(), "keys.yaml")
	loadTestKeyPopulation(t, s.keyStore, path, map[string]*keys.VirtualKey{id: record})
	return rawKey
}

// prehashedTestRecord preserves Create's incidental identity/lifecycle setup.
// Each record is private, while only the generated credential is shared.
func prehashedTestRecord(t *testing.T, id string, key keys.VirtualKey) (string, *keys.VirtualKey) {
	t.Helper()
	rawKey, hash := prehashedTestIdentity(t, id)
	key.ID, key.HashedKey, key.KeyEnv = id, hash, ""
	if key.UUID == "" {
		key.UUID = uuid.NewString()
	}
	key.CreatedAt = utils.NowUTC()
	key.UpdatedAt = key.CreatedAt
	return rawKey, &key
}

// seedFileTestKey adds an incidental key without dropping any existing keys.
func seedFileTestKey(t *testing.T, store *keys.FileKeyStore, path, id string, key keys.VirtualKey) string {
	t.Helper()
	population := store.List()
	require.NotContains(t, population, id, "duplicate fixture key")
	rawKey, record := prehashedTestRecord(t, id, key)
	population[id] = record
	loadTestKeyPopulation(t, store, path, population)
	return rawKey
}

func loadTestKeyPopulation(t *testing.T, store *keys.FileKeyStore, path string, population map[string]*keys.VirtualKey) {
	t.Helper()
	data, err := yaml.Marshal(struct {
		Version string                      `yaml:"version"`
		Keys    map[string]*keys.VirtualKey `yaml:"keys"`
	}{Version: "1.0", Keys: population})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	require.NoError(t, store.Load())
	require.Len(t, store.List(), len(population))
}
