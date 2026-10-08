package clientcli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

type fakeKeysAPI struct {
	created *pkgClient.CreateKeyRequest
	deleted string
}

func (f *fakeKeysAPI) ListKeys() ([]*pkgClient.KeyResponse, error) { return nil, nil }

func (f *fakeKeysAPI) CreateKey(req *pkgClient.CreateKeyRequest) (*pkgClient.CreateKeyResponse, error) {
	f.created = req
	resp := &pkgClient.CreateKeyResponse{}
	resp.Data.ID, resp.Data.Role, resp.Data.TeamID, resp.Data.RawKey = req.ID, "user", req.ID, "zzk-raw-key"
	return resp, nil
}

func (f *fakeKeysAPI) DeleteKey(id string) error {
	f.deleted = id
	return nil
}

// The flags become the request the server's create schema describes, and
// the name defaults to the id the server requires anyway.
func TestKeyCreateRequest(t *testing.T) {
	cmd := newKeysCreateCmd()
	require.NoError(t, cmd.ParseFlags([]string{
		"--team", "agents", "--team-role", "owner", "--rpm", "60", "--tpm", "1000",
		"--spend-limit", "5", "--reset-period", "daily", "--expires", "2027-01-01T00:00:00Z",
		"--default-max-tokens", "4096",
	}))

	req, err := keyCreateRequest(cmd, "ci")
	require.NoError(t, err)
	assert.Equal(t, "ci", req.Name)
	assert.Equal(t, "agents", req.TeamID)
	assert.Equal(t, "owner", req.TeamRole)
	assert.Equal(t, 60, req.RPMLimit)
	assert.Equal(t, 1000, req.TPMLimit)
	assert.InDelta(t, 5.0, req.SpendLimit, 0)
	assert.Equal(t, "daily", req.ResetPeriod)
	assert.Equal(t, 4096, req.DefaultMaxTokens)
	require.NotNil(t, req.ExpiresAt)
	assert.Equal(t, "2027-01-01T00:00:00Z", *req.ExpiresAt)
	assert.Empty(t, req.Role, "unset: the server's default applies")
}

// The key alone goes to stdout, so K=$(zzrouter keys create id) captures
// exactly it; what a person needs to read goes to stderr.
func TestKeysCreatePrintsOnlyTheKeyOnStdout(t *testing.T) {
	api := &fakeKeysAPI{}
	cmd := newKeysCreateCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	req, err := keyCreateRequest(cmd, "claude-code")
	require.NoError(t, err)

	require.NoError(t, runKeysCreate(cmd, api, req))

	assert.Equal(t, "zzk-raw-key\n", out.String())
	assert.Contains(t, errOut.String(), "shown only this once")
	assert.NotContains(t, errOut.String(), "zzk-raw-key")
}

func TestKeysRm(t *testing.T) {
	api := &fakeKeysAPI{}
	cmd, out := newTestCmd("")
	require.NoError(t, runKeysRm(cmd, api, "ci"))
	assert.Equal(t, "ci", api.deleted)
	assert.Contains(t, out.String(), "Deleted key ci")
}

func TestKeysCommandIsRegistered(t *testing.T) {
	keys, _, err := newRootCmd().Find([]string{"keys", "create"})
	require.NoError(t, err)
	assert.Equal(t, "create", keys.Name())
}

func TestKeyState(t *testing.T) {
	assert.Equal(t, "active", keyState(&pkgClient.KeyResponse{}))
	assert.Equal(t, "expired", keyState(&pkgClient.KeyResponse{IsExpired: true}))
	assert.Equal(t, "suspended", keyState(&pkgClient.KeyResponse{Suspended: true, IsExpired: true}),
		"suspension is the reason an operator acted on")
}
