package filestore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stretchr/testify/require"
)

func TestRouteAccessPersistence(t *testing.T) {
	for _, mode := range []string{"", "direct", "selected", "none"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "teams.yaml")
			store := NewTeams(path)
			policy := teams.RouteAccess{Mode: mode}
			if mode == "selected" {
				policy.Routes = []string{"ollama"}
			}
			require.NoError(t, store.Create("team", &teams.Team{Name: "Team", Kind: teams.KindShared, RouteAccess: policy}))
			require.NoError(t, store.Save())
			loaded := NewTeams(path)
			require.NoError(t, loaded.Load())
			require.Equal(t, policy, loaded.Get("team").RouteAccess)
			require.Equal(t, mode == "selected" || mode == "none", loaded.HasGatedTeam())
		})
	}
	for _, yaml := range []string{
		"teams:\n  a: {kind: shared, route_access: null}",
		"teams:\n  a: {kind: shared, route_access: {}}",
		"teams:\n  a: {kind: shared, route_access: {mode: select, routes: [ollama]}}",
		"teams:\n  a: {kind: shared, route_access: {mode: all, typo: x}}",
		"teams:\n  a: &a {kind: shared, route_access: &none null}\n  b: {kind: shared, route_access: *none}",
		"teams:\n  a: &a {kind: shared, route_access: null}\n  b: {<<: *a}",
	} {
		t.Run(yaml, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "teams.yaml")
			require.NoError(t, os.WriteFile(path, []byte(yaml), 0600))
			store := NewTeams(path)
			require.Error(t, store.Load())
			require.Empty(t, store.List())
		})
	}
}
