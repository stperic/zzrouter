package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A custom json.Unmarshaler takes over decoding for its type, which
// silently discards the DisallowUnknownFields that BindJSONStrict set on
// the outer decoder. `host` used to be swallowed here while
// /runs/preview rejected it, so a request meant for a worker resolved
// against the coordinator's own model store and came back 500.
func TestLaunchRunRequestRejectsUnknownField(t *testing.T) {
	var req LaunchRunRequest
	err := json.Unmarshal([]byte(`{
		"provider": "vllm",
		"launch_mode": "native",
		"model_name": "some-model",
		"host": "192.0.2.10"
	}`), &req)

	require.Error(t, err, "an unimplemented field must not be accepted silently")
	assert.Contains(t, err.Error(), "host")
}

// The alias the custom unmarshaller exists for has to keep working.
func TestLaunchRunRequestAcceptsModelAlias(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"model alias promotes", `{"provider":"vllm","launch_mode":"native","model":"aliased"}`, "aliased"},
		{"model_name still works", `{"provider":"vllm","launch_mode":"native","model_name":"direct"}`, "direct"},
		{"model_name wins over alias", `{"provider":"vllm","launch_mode":"native","model_name":"direct","model":"aliased"}`, "direct"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req LaunchRunRequest
			require.NoError(t, json.Unmarshal([]byte(tt.body), &req))
			assert.Equal(t, tt.want, req.Model)
		})
	}
}

// The node selector is `node`. Pinning it here because the docs got this
// wrong and cost a debugging session.
func TestLaunchRunRequestNodeSelector(t *testing.T) {
	var req LaunchRunRequest
	require.NoError(t, json.Unmarshal([]byte(
		`{"provider":"vllm","launch_mode":"native","model_name":"m","node":"worker-1"}`), &req))

	assert.Equal(t, "worker-1", req.Node)
}
