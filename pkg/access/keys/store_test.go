package keys

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateRawKey(t *testing.T) {
	t.Parallel()
	key, err := GenerateRawKey()
	require.NoError(t, err)
	assert.True(t, len(key) > len(KeyPrefix))
	assert.Equal(t, KeyPrefix, key[:len(KeyPrefix)])

	// Keys should be unique
	key2, err := GenerateRawKey()
	require.NoError(t, err)
	assert.NotEqual(t, key, key2)
}

func TestHashAndVerify(t *testing.T) {
	t.Parallel()
	rawKey := "zzr_test-key-for-hashing-purposes"

	hash, err := HashKey(rawKey)
	require.NoError(t, err)
	assert.Contains(t, hash, "$argon2id$")

	assert.True(t, VerifyKey(rawKey, hash))
	assert.False(t, VerifyKey("wrong-key", hash))
	assert.False(t, VerifyKey("", hash))
}

func TestVerifyKey_InvalidHash(t *testing.T) {
	t.Parallel()
	assert.False(t, VerifyKey("key", "not-a-hash"))
	assert.False(t, VerifyKey("key", "$argon2id$v=19$m=65536,t=1,p=2$bad"))
	assert.False(t, VerifyKey("key", ""))
}

func testKeysYAML(t *testing.T, rawKey string) string {
	t.Helper()
	hash, err := HashKey(rawKey)
	require.NoError(t, err)

	return `version: "1"

keys:
  team-dev:
    name: "Development Team"
    hashed_key: "` + hash + `"
    role: user
    team_id: engineering
    team_role: owner
    expires_at: "2030-12-31T23:59:59Z"
    max_parallel_requests: 10
    metadata:
      team: engineering
    rpm_limit: 100
    spend_limit: 10.00
    reset_period: monthly

  ci-pipeline:
    name: "CI Pipeline"
    key_env: TEST_CI_KEY
    role: user
    team_id: engineering
    team_role: member
`
}

func writeTestKeysFile(t *testing.T, dir, rawKey string) string {
	t.Helper()
	path := filepath.Join(dir, "keys.yaml")
	require.NoError(t, os.WriteFile(path, []byte(testKeysYAML(t, rawKey)), 0644))
	return path
}

func TestFileKeyStore_Load(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rawKey := "zzr_test-load-key-12345678901234567890"
	path := writeTestKeysFile(t, dir, rawKey)

	store := NewFileKeyStore(path)
	require.NoError(t, store.Load())

	keys := store.List()
	assert.Len(t, keys, 2)

	dev := store.Get("team-dev")
	require.NotNil(t, dev)
	assert.Equal(t, "Development Team", dev.Name)
	assert.Equal(t, "user", dev.Role)
	assert.Equal(t, "engineering", dev.TeamID)
	assert.Equal(t, "owner", dev.TeamRole)
	assert.Equal(t, 10, dev.MaxParallelRequests)
	assert.Equal(t, "engineering", dev.Metadata["team"])
	assert.Equal(t, 100, dev.RPMLimit)
	assert.Equal(t, 10.0, dev.SpendLimit)
	assert.Equal(t, "monthly", dev.ResetPeriod)
	require.NotNil(t, dev.ExpiresAt)
	assert.Equal(t, 2030, dev.ExpiresAt.Year())

	ci := store.Get("ci-pipeline")
	require.NotNil(t, ci)
	assert.Equal(t, "CI Pipeline", ci.Name)
	assert.Equal(t, "TEST_CI_KEY", ci.KeyEnv)
}

func TestFileKeyStore_ValidateRawKey_Hashed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rawKey := "zzr_validate-hashed-key-test-1234567890"
	path := writeTestKeysFile(t, dir, rawKey)

	store := NewFileKeyStore(path)
	require.NoError(t, store.Load())

	id, vk := store.ValidateRawKey(rawKey)
	assert.Equal(t, "team-dev", id)
	require.NotNil(t, vk)

	// Wrong key should not match
	id, vk = store.ValidateRawKey("wrong-key")
	assert.Empty(t, id)
	assert.Nil(t, vk)
}

func TestFileKeyStore_ValidateRawKey_Env(t *testing.T) {
	dir := t.TempDir()
	rawKey := "zzr_some-key-for-hashed-validation-test"
	path := writeTestKeysFile(t, dir, rawKey)

	store := NewFileKeyStore(path)
	require.NoError(t, store.Load())

	// Set env var
	t.Setenv("TEST_CI_KEY", "my-ci-secret")

	id, vk := store.ValidateRawKey("my-ci-secret")
	assert.Equal(t, "ci-pipeline", id)
	require.NotNil(t, vk)
}

func TestFileKeyStore_GetNotFound(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")
	assert.Nil(t, store.Get("nonexistent"))
}

func TestFileKeyStore_Create(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")

	vk := &VirtualKey{
		Name:     "Test Key",
		Role:     "user",
		TeamID:   "team-a",
		TeamRole: "owner",
	}

	rawKey, err := store.Create("test-key", vk)
	require.NoError(t, err)
	assert.True(t, len(rawKey) > 0)
	assert.Contains(t, rawKey, KeyPrefix)

	// Verify it was stored
	got := store.Get("test-key")
	require.NotNil(t, got)
	assert.Equal(t, "Test Key", got.Name)
	assert.NotEmpty(t, got.HashedKey)

	// Verify the raw key validates
	id, matched := store.ValidateRawKey(rawKey)
	assert.Equal(t, "test-key", id)
	assert.NotNil(t, matched)
}

func TestFileKeyStore_CreateDuplicate(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")
	vk := &VirtualKey{Name: "Key1", Role: "user", TeamID: "t", TeamRole: "owner"}

	_, err := store.Create("dup", vk)
	require.NoError(t, err)

	_, err = store.Create("dup", vk)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestFileKeyStore_Update(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")
	vk := &VirtualKey{Name: "Original", Role: "user", TeamID: "t", TeamRole: "owner"}
	_, err := store.Create("upd", vk)
	require.NoError(t, err)

	updated := &VirtualKey{
		Name: "Updated",
		Role: "admin",
	}
	require.NoError(t, store.Update("upd", updated))

	got := store.Get("upd")
	require.NotNil(t, got)
	assert.Equal(t, "Updated", got.Name)
	assert.Equal(t, "admin", got.Role)
	// HashedKey and team binding should be preserved from original
	assert.NotEmpty(t, got.HashedKey)
	assert.Equal(t, "t", got.TeamID)
	assert.Equal(t, "owner", got.TeamRole)
}

func TestFileKeyStore_UpdateNotFound(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")
	err := store.Update("missing", &VirtualKey{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestFileKeyStore_Delete(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")
	vk := &VirtualKey{Name: "ToDelete", Role: "user", TeamID: "t", TeamRole: "owner"}
	_, err := store.Create("del", vk)
	require.NoError(t, err)

	require.NoError(t, store.Delete("del"))
	assert.Nil(t, store.Get("del"))
}

func TestFileKeyStore_DeleteNotFound(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")
	err := store.Delete("missing")
	assert.Error(t, err)
}

func TestFileKeyStore_RotateKey(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")
	vk := &VirtualKey{Name: "Rotate", Role: "user", TeamID: "t", TeamRole: "owner"}
	oldRaw, err := store.Create("rot", vk)
	require.NoError(t, err)

	newRaw, err := store.RotateKey("rot")
	require.NoError(t, err)
	assert.NotEqual(t, oldRaw, newRaw)

	// Old key should no longer work
	id, _ := store.ValidateRawKey(oldRaw)
	assert.Empty(t, id)

	// New key should work
	id, _ = store.ValidateRawKey(newRaw)
	assert.Equal(t, "rot", id)
}

func TestFileKeyStore_SaveAndReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.yaml")

	store := NewFileKeyStore(path)
	vk := &VirtualKey{
		Name:     "Persisted",
		Role:     "user",
		TeamID:   "t",
		TeamRole: "owner",
	}
	rawKey, err := store.Create("persist", vk)
	require.NoError(t, err)

	// Save to disk
	require.NoError(t, store.Save())

	// Load into new store
	store2 := NewFileKeyStore(path)
	require.NoError(t, store2.Load())

	got := store2.Get("persist")
	require.NotNil(t, got)
	assert.Equal(t, "Persisted", got.Name)

	// Validate key still works after reload
	id, _ := store2.ValidateRawKey(rawKey)
	assert.Equal(t, "persist", id)
}

func TestVirtualKey_IsExpired(t *testing.T) {
	t.Parallel()
	// Not expired
	future := time.Now().Add(24 * time.Hour)
	vk := &VirtualKey{ExpiresAt: &future}
	assert.False(t, vk.IsExpired())

	// Expired
	past := time.Now().Add(-24 * time.Hour)
	vk = &VirtualKey{ExpiresAt: &past}
	assert.True(t, vk.IsExpired())

	// No expiration
	vk = &VirtualKey{}
	assert.False(t, vk.IsExpired())
}

func TestFileKeyStore_LoadValidationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing name",
			yaml:    "version: \"1\"\nkeys:\n  bad:\n    hashed_key: \"x\"\n    role: user\n    team_id: t\n    team_role: owner\n",
			wantErr: "missing name",
		},
		{
			name:    "missing key source",
			yaml:    "version: \"1\"\nkeys:\n  bad:\n    name: Bad\n    role: user\n    team_id: t\n    team_role: owner\n",
			wantErr: "must have hashed_key or key_env",
		},
		{
			name:    "missing team_id",
			yaml:    "version: \"1\"\nkeys:\n  bad:\n    name: Bad\n    hashed_key: x\n    role: user\n    team_role: owner\n",
			wantErr: "missing team_id",
		},
		{
			name:    "missing team_role",
			yaml:    "version: \"1\"\nkeys:\n  bad:\n    name: Bad\n    hashed_key: x\n    role: user\n    team_id: t\n",
			wantErr: "missing team_role",
		},
		{
			// Rejecting allowed_models on keys guards against silent
			// permission upgrades: if we let this field through, the key
			// would inherit its team's (likely wider) allow-list instead
			// of the caller's intended restriction.
			name:    "allowed_models on key rejected",
			yaml:    "version: \"1\"\nkeys:\n  bad:\n    name: Bad\n    hashed_key: x\n    role: user\n    team_id: t\n    team_role: owner\n    allowed_models:\n      - fast-chat\n",
			wantErr: "allowed_models on keys is not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewFileKeyStore("")
			err := store.loadFromBytes([]byte(tt.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestFileKeyStore_KeyModelAllowListIsRejectedBeforeUse(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keys.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: "1"
keys:
  realtime-key:
    name: Realtime key
    hashed_key: synthetic-test-hash
    team_id: restricted
    team_role: member
    allowed_models:
      - realtime /v1/realtime
`), 0o600))
	store := NewFileKeyStore(path)
	err := store.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed_models on keys is not supported; set model access on the team and remove this field from the key")
	assert.Nil(t, store.Get("realtime-key"), "an unsupported key policy must never become usable")
}
