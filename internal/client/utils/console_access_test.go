package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestConsoleHandoffExchangesAuthorityWithoutPuttingCodeInURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/zzrouter/v1/access/console-grants", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": ConsoleGrant{Token: "synthetic-code", Purpose: "session", URL: "https://router.example/console/", ExpiresAt: utils.NowUTC().Add(time.Minute)}})
	}))
	t.Cleanup(server.Close)
	client := NewClient(config.ClientNodeConfig{Address: server.URL, APIKey: "synthetic-test-key"})
	grant, err := client.BeginConsoleHandoff(t.Context(), ConsoleTarget{Page: "access", Item: "teams/one"})
	require.NoError(t, err)
	require.Equal(t, "synthetic-code", grant.Token)
	require.Equal(t, "https://router.example/console/#access?item=teams%2Fone", grant.URL)
	require.NotContains(t, grant.URL, grant.Token)
}

func TestConsoleHandoffRefusesUnsafeEntry(t *testing.T) {
	for _, target := range []string{"https://user:pass@router.example/console/", "https://router.example/console/?token=secret", "https://router.example/console/#token", "https://router.example/other", "http://router.example/console/", "javascript:alert(1)"} {
		t.Run(target, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": ConsoleGrant{Token: strings.Repeat("x", 32), Purpose: "setup", URL: target, ExpiresAt: utils.NowUTC().Add(time.Minute)}})
			}))
			t.Cleanup(server.Close)
			client := NewClient(config.ClientNodeConfig{Address: server.URL})
			_, err := client.BeginConsoleHandoff(t.Context(), ConsoleTarget{})
			require.Error(t, err)
		})
	}
}
