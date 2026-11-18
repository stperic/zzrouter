package filestore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKeysYAML(t *testing.T, rawKey string) string {
	t.Helper()
	hash, err := keys.HashKey(rawKey)
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

func writeTestKeysFile(t *testing.T, dir, rawKey string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keys.yaml"), []byte(testKeysYAML(t, rawKey)), 0644))
}

func TestFileKeyStore_Load(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rawKey := "zzr_test-load-key-12345678901234567890"
	writeTestKeysFile(t, dir, rawKey)

	snap := openTestStore(t, dir).Snapshot()
	count := 0
	for range snap.Keys() {
		count++
	}
	assert.Equal(t, 2, count)

	dev := snap.Key("team-dev")
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

	ci := snap.Key("ci-pipeline")
	require.NotNil(t, ci)
	assert.Equal(t, "CI Pipeline", ci.Name)
	assert.Equal(t, "TEST_CI_KEY", ci.KeyEnv)
}

func TestFileKeyStore_ValidateRawKey_Hashed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rawKey := "zzr_validate-hashed-key-test-1234567890"
	writeTestKeysFile(t, dir, rawKey)
	snap := openTestStore(t, dir).Snapshot()

	id, vk := snap.ValidateRawKey(rawKey)
	assert.Equal(t, "team-dev", id)
	require.NotNil(t, vk)

	// Wrong key should not match
	id, vk = snap.ValidateRawKey("wrong-key")
	assert.Empty(t, id)
	assert.Nil(t, vk)
}

func TestFileKeyStore_ValidateRawKey_Env(t *testing.T) {
	dir := t.TempDir()
	rawKey := "zzr_some-key-for-hashed-validation-test"
	writeTestKeysFile(t, dir, rawKey)
	snap := openTestStore(t, dir).Snapshot()

	// Set env var
	t.Setenv("TEST_CI_KEY", "my-ci-secret")

	id, vk := snap.ValidateRawKey("my-ci-secret")
	assert.Equal(t, "ci-pipeline", id)
	require.NotNil(t, vk)
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
			_, err := DecodeKeys([]byte(tt.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestFileKeyStore_KeyModelAllowListIsRejectedBeforeUse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keys.yaml"), []byte(`version: "1"
keys:
  realtime-key:
    name: Realtime key
    hashed_key: synthetic-test-hash
    team_id: restricted
    team_role: member
    allowed_models:
      - realtime /v1/realtime
`), 0o600))
	store, err := Open(t.Context(), dir, Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed_models on keys is not supported; set model access on the team and remove this field from the key")
	assert.Nil(t, store, "an unsupported key policy must never become usable")
}
