package filestore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/access/teams"
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
	path := filepath.Join(dir, "teams.yaml")
	require.NoError(t, os.WriteFile(path, []byte(testTeamsYAML), 0644))

	store := NewTeams(path)
	require.NoError(t, store.Load())

	all := store.List()
	assert.Len(t, all, 2)

	eng := store.Get("engineering")
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
	assert.True(t, store.HasGatedTeam())
}

func TestFileTeamStore_LoadRejectsMissingKind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "teams.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: "1"
teams:
  bad:
    name: Bad
`), 0644))
	store := NewTeams(path)
	err := store.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing kind")
}

func TestFileTeamStore_GetNotFound(t *testing.T) {
	t.Parallel()
	store := NewTeams("")
	assert.Nil(t, store.Get("nonexistent"))
}

func TestFileTeamStore_Create(t *testing.T) {
	t.Parallel()
	store := NewTeams("")

	team := &teams.Team{
		Name:          "New Team",
		Kind:          teams.KindShared,
		AllowedModels: []string{"fast-chat"},
	}

	require.NoError(t, store.Create("new-team", team))

	got := store.Get("new-team")
	require.NotNil(t, got)
	assert.Equal(t, "New Team", got.Name)
	assert.Equal(t, "new-team", got.ID)
	assert.Equal(t, teams.KindShared, got.Kind)
	assert.NotEmpty(t, got.UUID)
	assert.False(t, got.CreatedAt.IsZero())

	// Gated-team recompute after create.
	assert.True(t, store.HasGatedTeam())
}

func TestFileTeamStore_CreateRejectsMissingKind(t *testing.T) {
	t.Parallel()
	store := NewTeams("")
	err := store.Create("x", &teams.Team{Name: "Bad"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kind is required")
}

func TestFileTeamStore_CreateDuplicate(t *testing.T) {
	t.Parallel()
	store := NewTeams("")
	require.NoError(t, store.Create("dup", &teams.Team{Name: "Dup", Kind: teams.KindShared}))

	err := store.Create("dup", &teams.Team{Name: "Dup2", Kind: teams.KindShared})
	assert.ErrorIs(t, err, teams.ErrTeamExists)
}

func TestFileTeamStore_Update(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	restore := utils.SetClock(utils.FixedClock(now))
	defer restore()
	store := NewTeams("")
	require.NoError(t, store.Create("upd", &teams.Team{Name: "Original", Kind: teams.KindShared}))

	original := store.Get("upd")
	originalUUID := original.UUID

	updated := &teams.Team{
		Name:          "Updated",
		AllowedModels: []string{"code-gen"},
	}
	_ = utils.SetClock(utils.FixedClock(now.Add(time.Second)))
	require.NoError(t, store.Update("upd", updated))

	got := store.Get("upd")
	assert.Equal(t, "Updated", got.Name)
	assert.Equal(t, originalUUID, got.UUID)     // preserved
	assert.Equal(t, teams.KindShared, got.Kind) // preserved from existing
	assert.True(t, got.UpdatedAt.After(got.CreatedAt))
}

func TestFileTeamStore_UpdateNotFound(t *testing.T) {
	t.Parallel()
	store := NewTeams("")
	err := store.Update("missing", &teams.Team{})
	assert.ErrorIs(t, err, teams.ErrTeamNotFound)
}

func TestFileTeamStore_Delete(t *testing.T) {
	t.Parallel()
	store := NewTeams("")
	require.NoError(t, store.Create("del", &teams.Team{
		Name:          "ToDelete",
		Kind:          teams.KindShared,
		AllowedModels: []string{"x"},
	}))
	assert.True(t, store.HasGatedTeam())

	require.NoError(t, store.Delete("del"))
	assert.Nil(t, store.Get("del"))
	assert.False(t, store.HasGatedTeam(), "gated flag must recompute after delete")
}

func TestFileTeamStore_DeleteNotFound(t *testing.T) {
	t.Parallel()
	store := NewTeams("")
	err := store.Delete("missing")
	assert.ErrorIs(t, err, teams.ErrTeamNotFound)
}

func TestFileTeamStore_SaveAndReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "teams.yaml")

	store := NewTeams(path)
	require.NoError(t, store.Create("persist", &teams.Team{
		Name:          "Persisted",
		Kind:          teams.KindShared,
		AllowedModels: []string{"fast-chat"},
	}))

	require.NoError(t, store.Save())

	// Load into new store
	store2 := NewTeams(path)
	require.NoError(t, store2.Load())

	got := store2.Get("persist")
	require.NotNil(t, got)
	assert.Equal(t, "Persisted", got.Name)
	assert.Equal(t, teams.KindShared, got.Kind)
	assert.Equal(t, []string{"fast-chat"}, got.AllowedModels)
}
