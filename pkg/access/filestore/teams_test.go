package filestore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTeamsYAML = `version: "1"

teams:
  engineering:
    uuid: "550e8400-e29b-41d4-a716-446655440000"
    name: "Engineering"
    kind: shared
    allowed_models:
      - fast-chat
      - code-gen
    rpm_limit: 100
    tpm_limit: 500000
    spend_limit: 50.0
    reset_period: monthly
    metadata:
      department: product

  research:
    uuid: "660e8400-e29b-41d4-a716-446655440001"
    name: "Research"
    kind: shared
`

func TestFileTeamStore_Load(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "teams.yaml"), []byte(testTeamsYAML), 0644))

	snap := openTestStore(t, dir).Snapshot()
	count := 0
	for range snap.Teams() {
		count++
	}
	assert.Equal(t, 2, count)

	eng := snap.Team("engineering")
	require.NotNil(t, eng)
	assert.Equal(t, "Engineering", eng.Name)
	assert.Equal(t, "engineering", eng.ID)
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000", eng.UUID)
	assert.Equal(t, teams.KindShared, eng.Kind)
	assert.Equal(t, []string{"fast-chat", "code-gen"}, eng.AllowedModels)
	assert.Equal(t, 100, eng.RPMLimit)
	assert.Equal(t, 500000, eng.TPMLimit)
	assert.Equal(t, 50.0, eng.SpendLimit)
	assert.Equal(t, "monthly", eng.ResetPeriod)
	assert.Equal(t, "product", eng.Metadata["department"])

	// HasGatedTeam is a hot-path gate on the anonymous /v1/* guard — Load
	// must refresh it, otherwise unauthenticated traffic bypasses model access.
	assert.True(t, snap.HasGatedTeam())
}

func TestFileTeamStore_LoadRejectsMissingKind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "teams.yaml"), []byte(`version: "1"
teams:
  bad:
    name: Bad
`), 0644))
	_, err := Open(t.Context(), dir, Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing kind")
}

func TestFileTeamStore_Update(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	restore := utils.SetClock(utils.FixedClock(now))
	defer restore()
	store := openTestStore(t, t.TempDir())
	original, err := store.CreateTeam(t.Context(), "upd", teams.Team{Name: "Original", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)

	_ = utils.SetClock(utils.FixedClock(now.Add(time.Second)))
	_, err = store.UpdateTeam(t.Context(), "upd", func(team *teams.Team) error {
		team.Name, team.AllowedModels, team.Kind = "Updated", []string{"code-gen"}, ""
		return nil
	}, audit.Event{})
	require.NoError(t, err)

	got := store.Snapshot().Team("upd")
	assert.Equal(t, "Updated", got.Name)
	assert.Equal(t, original.UUID, got.UUID)    // preserved
	assert.Equal(t, teams.KindShared, got.Kind) // preserved from existing
	assert.True(t, got.UpdatedAt.After(got.CreatedAt))
}
