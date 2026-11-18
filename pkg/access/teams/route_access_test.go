package teams

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
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
