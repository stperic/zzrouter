package teams

import (
	"encoding/json"
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
