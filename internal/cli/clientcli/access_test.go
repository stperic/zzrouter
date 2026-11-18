package clientcli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/access/localproof"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func recoveryFixture(t *testing.T, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	authority, err := localproof.New(t.Context(), dir, localproof.File{Endpoint: endpoint, ConsoleURL: "https://router.example/console/"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, authority.Close()) })
	return dir
}

func TestLocalOwnerRecoveryUsesOnlySelectedInstallation(t *testing.T) {
	var dir string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/zzrouter/v1/access/recovery/local", r.URL.Path)
		require.Empty(t, r.Header.Get("Origin"))
		require.Empty(t, r.Header.Get("Authorization"))
		var input struct{ Proof string }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		file, err := localproof.Read(r.Context(), dir)
		require.NoError(t, err)
		require.Equal(t, file.Proof, input.Proof)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": localRecoveryGrant{URL: file.ConsoleURL, Token: "synthetic-once-only-code", ExpiresAt: utils.NowUTC().Add(time.Minute)}})
	}))
	t.Cleanup(server.Close)
	dir = recoveryFixture(t, server.URL)
	grant, err := recoverLocalOwner(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, "synthetic-once-only-code", grant.Token)
	_, err = recoverLocalOwner(t.Context(), t.TempDir())
	require.ErrorContains(t, err, "read local recovery authority")
}

func TestOwnerRecoveryCommandUsesSelectedInstallation(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	var dir string
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/zzrouter/v1/access/recovery/local", r.URL.Path)
		require.Empty(t, r.Header.Get("Authorization"))
		var input struct{ Proof string }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		file, err := localproof.Read(r.Context(), dir)
		require.NoError(t, err)
		require.Equal(t, file.Proof, input.Proof)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": localRecoveryGrant{URL: file.ConsoleURL, Token: "synthetic-recovery-code", ExpiresAt: utils.NowUTC().Add(time.Minute)}})
	}))
	t.Cleanup(server.Close)
	dir = recoveryFixture(t, server.URL)
	var output bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"access", "recover-owner", "--dir", dir})
	require.NoError(t, cmd.Execute())
	require.EqualValues(t, 1, requests.Load())
	require.Contains(t, output.String(), "https://router.example/console/")
	require.Contains(t, output.String(), "synthetic-recovery-code")
	file, err := localproof.Read(t.Context(), dir)
	require.NoError(t, err)
	require.NotContains(t, output.String(), file.Proof)
}

func TestLocalOwnerRecoveryNeverFollowsCrossOriginRedirect(t *testing.T) {
	var received atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Add(1) }))
	t.Cleanup(other.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	_, err := recoverLocalOwner(t.Context(), recoveryFixture(t, server.URL))
	require.Error(t, err)
	require.Zero(t, received.Load(), "private proof must stay on the selected node")
}

func TestLocalOwnerRecoveryRefusesRemoteTargetsAndInvalidGrants(t *testing.T) {
	for _, endpoint := range []string{"https://remote.example:9090", "http://203.0.113.20:9090", "http://user:pass@127.0.0.1:9090", "http://127.0.0.1:9090/?x=1", "ftp://127.0.0.1:9090"} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := recoverLocalOwner(t.Context(), recoveryFixture(t, endpoint))
			require.ErrorContains(t, err, "literal local address")
		})
	}
	for _, response := range []string{
		`{"data":{"url":"https://other.example/console/","token":"code","expires_at":"2030-01-01T00:00:00Z"}}`,
		`{"data":{"url":"https://router.example/console/","token":"","expires_at":"2030-01-01T00:00:00Z"}}`,
		strings.Repeat(" ", 8193),
	} {
		t.Run("invalid response", func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response)) }))
			t.Cleanup(server.Close)
			_, err := recoverLocalOwner(t.Context(), recoveryFixture(t, server.URL))
			require.Error(t, err)
		})
	}
}
