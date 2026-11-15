package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestConsoleDiscoveryUsesConfiguredPublicOriginAndEncodedTarget(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		require.Equal(t, "/.well-known/oauth-authorization-server", r.URL.Path)
		require.Equal(t, http.MethodGet, r.Method)
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "https://router.example:9443"})
	}))
	defer server.Close()
	client := NewClient(pkgConfig.ClientNodeConfig{Address: server.URL, APIKey: "synthetic-test-key"})
	got, err := client.ConsoleURL(t.Context(), ConsoleTarget{Page: "access", Item: "teams/Research & design?#"})
	require.NoError(t, err)
	require.Equal(t, "https://router.example:9443/console/#access?item=teams%2FResearch+%26+design%3F%23", got)
	require.EqualValues(t, 1, requests.Load())
	require.NotContains(t, got, "synthetic-test-key")
	// The browser passes the hash query directly to URLSearchParams, not through
	// a second fragment URL decoder.
	query, err := url.ParseQuery(strings.SplitN(got, "?", 2)[1])
	require.NoError(t, err)
	require.Equal(t, "teams/Research & design?#", query.Get("item"))
}

func TestConsoleOriginValidation(t *testing.T) {
	for _, origin := range []string{"", "/relative", "ftp://example.org", "javascript:alert(1)", "https://user:pass@example.org", "https://example.org/path", "https://example.org?key=secret", "https://example.org?", "https://example.org#fragment", "https://example.org#", "https://", "https://example.org/%2f", "https://router.example/", "https://example.org:99999", "https://example.org:", "http://remote.example"} {
		t.Run(origin, func(t *testing.T) { _, err := consoleTargetURL(origin, ConsoleTarget{}); require.Error(t, err) })
	}
	for _, origin := range []string{"http://localhost:9090", "https://router.example", "http://[::1]:9090"} {
		got, err := consoleTargetURL(origin, ConsoleTarget{})
		require.NoError(t, err)
		require.Equal(t, strings.TrimSuffix(origin, "/")+"/console/#overview", got)
	}
	_, err := consoleTargetURL("https://example.org", ConsoleTarget{Page: "unknown"})
	require.Error(t, err)
	_, err = consoleTargetURL("https://example.org", ConsoleTarget{Page: "access", Mode: "unknown"})
	require.Error(t, err)
}

func TestConsoleTargetsMatchShellDestinations(t *testing.T) {
	for _, page := range []string{"overview", "chat", "models", "nodes", "workloads", "logs", "clients", "providers", "routes", "access", "connections", "settings"} {
		got, err := consoleTargetURL("https://router.example", ConsoleTarget{Page: page})
		require.NoError(t, err)
		require.Equal(t, "https://router.example/console/#"+page, got)
	}
	for _, item := range []string{"updates", "pricing", "setup"} {
		got, err := consoleTargetURL("https://router.example", ConsoleTarget{Page: "settings", Item: item})
		require.NoError(t, err)
		require.Equal(t, "https://router.example/console/#settings?item="+item, got)
		_, err = consoleTargetURL("https://router.example", ConsoleTarget{Page: item})
		require.Error(t, err)
	}
}

func TestConsoleUnavailableAndCancellation(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMisdirectedRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			client := NewClient(pkgConfig.ClientNodeConfig{Address: server.URL})
			got, err := client.ConsoleURL(t.Context(), ConsoleTarget{})
			require.Empty(t, got)
			require.ErrorContains(t, err, "connect to the coordinator")
		})
	}
	client := NewClient(pkgConfig.ClientNodeConfig{Address: "http://127.0.0.1:1"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := client.ConsoleURL(ctx, ConsoleTarget{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestConsoleDiscoveryDoesNotFollowCrossOriginRedirect(t *testing.T) {
	var received atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { received.Add(1) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := NewClient(pkgConfig.ClientNodeConfig{Address: origin.URL, APIKey: "synthetic-test-key"})
	_, err := client.ConsoleURL(t.Context(), ConsoleTarget{})
	require.Error(t, err)
	require.Zero(t, received.Load())
}
