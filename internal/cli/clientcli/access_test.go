package clientcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
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

// requireLocalProofRequest checks the proof arrives in its header only, with an empty JSON body.
func requireLocalProofRequest(t *testing.T, r *http.Request, dir string) *localproof.File {
	t.Helper()
	file, err := localproof.Read(r.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, []string{file.Proof}, r.Header.Values(localproof.Header))
	require.Equal(t, "application/json", r.Header.Get("Content-Type"))
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(body))
	require.NotContains(t, r.URL.String(), file.Proof)
	return file
}

func TestLocalOwnerRecoveryUsesOnlySelectedInstallation(t *testing.T) {
	var dir string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/zzrouter/v1/access/recovery/local", r.URL.Path)
		require.Empty(t, r.Header.Get("Origin"))
		require.Empty(t, r.Header.Get("Authorization"))
		file := requireLocalProofRequest(t, r, dir)
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
		file := requireLocalProofRequest(t, r, dir)
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

// The session talks only to the installation's literal local endpoint: no
// environment proxy is consulted, even for a non-loopback address of this host.
func TestLocalSessionReachesOnlyTheInstalledNode(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	host := "127.0.0.1"
	if addresses, err := net.InterfaceAddrs(); err == nil {
		for _, address := range addresses {
			if network, ok := address.(*net.IPNet); ok && network.IP.To4() != nil && !network.IP.IsLoopback() && !network.IP.IsLinkLocalUnicast() {
				host = network.IP.String()
				break
			}
		}
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	require.NoError(t, err)
	var dir string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireLocalProofRequest(t, r, dir)
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	dir = recoveryFixture(t, server.URL)
	session, err := openLocal(t.Context(), dir)
	require.NoError(t, err)
	defer session.client.CloseIdleConnections()
	require.Nil(t, session.client.Transport.(*http.Transport).Proxy)
	req, err := session.request(t.Context(), http.MethodPost, "/zzrouter/v1/access/recovery/local", []byte("{}"))
	require.NoError(t, err)
	response, err := session.client.Do(req)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNoContent, response.StatusCode, "reached %s directly", host)
	require.Zero(t, proxied.Load(), "no proxy received the local proof")
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
