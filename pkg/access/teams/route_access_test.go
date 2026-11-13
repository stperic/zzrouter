package teams

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRouteAccessJSON(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"mode":"selected"}`, `{"mode":"selected","routes":["ollama","ollama"]}`, `{"mode":"selected","routes":[" ollama"]}`, `{"mode":"none","routes":["ollama"]}`, `{"mode":"al"}`, `{"mode":"direct","provider":["ollama"]}`} {
		t.Run(body, func(t *testing.T) { var policy RouteAccess; require.Error(t, json.Unmarshal([]byte(body), &policy)) })
	}
	var policy RouteAccess
	require.NoError(t, json.Unmarshal([]byte(`{"mode":"selected","routes":["ollama","groq"]}`), &policy))
	require.True(t, policy.Allows("ollama"))
	require.False(t, policy.Allows("vllm"))
	require.False(t, policy.Allows(""))
	clone := policy.Clone()
	clone.Routes[0] = "other"
	require.True(t, policy.Allows("ollama"))
	type request struct {
		Access RouteAccess `json:"route_access,omitzero"`
	}
	body, err := json.Marshal(request{Access: RouteAccess{Routes: []string{}}})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(body))
	body, err = json.Marshal(RouteAccess{})
	require.NoError(t, err)
	require.JSONEq(t, `{"mode":"direct"}`, string(body))
}

func TestRouteAccessPersistence(t *testing.T) {
	for _, mode := range []string{"", "direct", "selected", "none"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "teams.yaml")
			store := NewFileTeamStore(path)
			policy := RouteAccess{Mode: mode}
			if mode == "selected" {
				policy.Routes = []string{"ollama"}
			}
			require.NoError(t, store.Create("team", &Team{Name: "Team", Kind: KindShared, RouteAccess: policy}))
			require.NoError(t, store.Save())
			loaded := NewFileTeamStore(path)
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
			store := NewFileTeamStore(path)
			require.Error(t, store.Load())
			require.Empty(t, store.List())
		})
	}
}
