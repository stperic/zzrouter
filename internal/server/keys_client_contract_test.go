package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// The CLI's create request binds on the server, which refuses unknown
// fields: a field the client sends under a name the server does not
// bind is a 400, not a silently dropped value.
func TestClientCreateKeyRequestBindsOnTheServer(t *testing.T) {
	expires := "2027-01-01T00:00:00Z"
	sent := pkgClient.CreateKeyRequest{
		ID: "ci", Name: "CI", Description: "d", Role: "user", TeamID: "t", TeamRole: "member",
		ExpiresAt: &expires, MaxParallelRequests: 1, RPMLimit: 2, TPMLimit: 3, SpendLimit: 4,
		ResetPeriod: "daily", DefaultMaxTokens: 5, Metadata: map[string]string{"k": "v"},
	}
	raw, err := json.Marshal(sent)
	require.NoError(t, err)

	var bound createKeyHTTPRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&bound))

	again, err := json.Marshal(bound)
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(again), "every value the client sends is bound")
}
