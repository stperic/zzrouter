package filestore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stretchr/testify/require"
)

func TestProviderAccessPersistence(t *testing.T) {
	for _, mode := range []string{"", "all", "selected", "none"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			policy := teams.ProviderAccess{Mode: mode}
			if mode == "selected" {
				policy.Providers = []string{"ollama"}
			}
			_, err := openTestStore(t, dir).CreateTeam(t.Context(), "team", teams.Team{Name: "Team", Kind: teams.KindShared, ProviderAccess: policy}, audit.Event{})
			require.NoError(t, err)
			loaded := openTestStore(t, dir).Snapshot()
			require.Equal(t, policy, loaded.Team("team").ProviderAccess)
			require.Equal(t, mode == "selected" || mode == "none", loaded.HasGatedTeam())
		})
	}
	for _, yaml := range []string{
		"teams:\n  a: {kind: shared, provider_access: null}",
		"teams:\n  a: {kind: shared, provider_access: {}}",
		"teams:\n  a: {kind: shared, provider_access: {mode: select, providers: [ollama]}}",
		"teams:\n  a: {kind: shared, provider_access: {mode: all, typo: x}}",
		"teams:\n  a: &a {kind: shared, provider_access: &none null}\n  b: {kind: shared, provider_access: *none}",
		"teams:\n  a: &a {kind: shared, provider_access: null}\n  b: {<<: *a}",
	} {
		t.Run(yaml, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "teams.yaml"), []byte(yaml), 0600))
			store, err := Open(t.Context(), dir, Options{})
			require.Error(t, err)
			require.Nil(t, store)
		})
	}
}
