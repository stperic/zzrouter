package teams

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderAccessJSON(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"mode":"selected"}`, `{"mode":"selected","providers":["ollama","ollama"]}`, `{"mode":"selected","providers":[" ollama"]}`, `{"mode":"none","providers":["ollama"]}`, `{"mode":"al"}`, `{"mode":"all","provider":["ollama"]}`} {
		t.Run(body, func(t *testing.T) { var policy ProviderAccess; require.Error(t, json.Unmarshal([]byte(body), &policy)) })
	}
	var policy ProviderAccess
	require.NoError(t, json.Unmarshal([]byte(`{"mode":"selected","providers":["ollama","groq"]}`), &policy))
	require.True(t, policy.Allows("ollama"))
	require.False(t, policy.Allows("vllm"))
	require.False(t, policy.Allows(""))
	clone := policy.Clone()
	clone.Providers[0] = "other"
	require.True(t, policy.Allows("ollama"))
	type request struct {
		Access ProviderAccess `json:"provider_access,omitzero"`
	}
	body, err := json.Marshal(request{Access: ProviderAccess{Providers: []string{}}})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(body))
	body, err = json.Marshal(ProviderAccess{})
	require.NoError(t, err)
	require.JSONEq(t, `{"mode":"all"}`, string(body))
}

func TestProviderAccessPersistence(t *testing.T) {
	for _, mode := range []string{"", "all", "selected", "none"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "teams.yaml")
			store := NewFileTeamStore(path)
			policy := ProviderAccess{Mode: mode}
			if mode == "selected" {
				policy.Providers = []string{"ollama"}
			}
			require.NoError(t, store.Create("team", &Team{Name: "Team", Kind: KindShared, ProviderAccess: policy}))
			require.NoError(t, store.Save())
			loaded := NewFileTeamStore(path)
			require.NoError(t, loaded.Load())
			require.Equal(t, policy, loaded.Get("team").ProviderAccess)
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
			path := filepath.Join(t.TempDir(), "teams.yaml")
			require.NoError(t, os.WriteFile(path, []byte(yaml), 0600))
			store := NewFileTeamStore(path)
			require.Error(t, store.Load())
			require.Empty(t, store.List())
		})
	}
}
