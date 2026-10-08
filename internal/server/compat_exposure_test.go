package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One rule decides whether a keyless request gets in, and every surface
// it guards answers the same way, with the reason, while the discovery
// document says so beforehand.
func TestAnonymousRefusal_OneRuleForEverySurface(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	surfaces := []string{"/v1/models", "/api/tags"}
	discovery := func() string {
		resp := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1", TestAdminKey, nil)
		require.Equal(t, http.StatusOK, resp.Code)
		var env struct {
			Data apiDiscoveryResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(resp.Body, &env))
		return env.Data.Authentication.CompatibleAPIs
	}

	for _, path := range surfaces {
		assert.NotEqual(t, http.StatusUnauthorized, makeRequest(t, s, TestRequest{Method: http.MethodGet, Path: path}).Code, path)
	}
	assert.True(t, strings.HasPrefix(discovery(), "optional:"))

	s.config.Auth.RequireCompatAuth = true
	for _, path := range surfaces {
		resp := makeRequest(t, s, TestRequest{Method: http.MethodGet, Path: path})
		assert.Equal(t, http.StatusUnauthorized, resp.Code, path)
		assert.Contains(t, string(resp.Body), "auth.require_compat_auth", path)
	}
	assert.Contains(t, discovery(), "require_compat_auth")

	// A team with a model allow-list closes the door the same way: a
	// caller with no key cannot be held to it.
	s.config.Auth.RequireCompatAuth = false
	resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey,
		map[string]any{"id": "gated", "name": "Gated", "allowed_models": []string{"some-model"}})
	require.Less(t, resp.Code, http.StatusMultipleChoices, string(resp.Body))
	for _, path := range surfaces {
		resp := makeRequest(t, s, TestRequest{Method: http.MethodGet, Path: path})
		assert.Equal(t, http.StatusUnauthorized, resp.Code, path)
		assert.Contains(t, string(resp.Body), "limits which models", path)
	}
	assert.Contains(t, discovery(), "limits which models")

	// A key still gets in.
	assert.NotEqual(t, http.StatusUnauthorized, makeAuthRequest(t, s, http.MethodGet, "/v1/models", TestAdminKey, nil).Code)
}

func TestWarnIfCompatOpenToNetwork(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cases := []struct {
		name    string
		ip      net.IP
		require bool
		warn    bool
	}{
		{"all interfaces, anonymous", net.IPv4zero, false, true},
		{"loopback", net.IPv4(127, 0, 0, 1), false, false},
		{"all interfaces, key required", net.IPv4zero, true, false},
	}
	for _, tc := range cases {
		logs.Reset()
		s.config.Auth.RequireCompatAuth = tc.require
		s.warnIfCompatOpenToNetwork(&net.TCPAddr{IP: tc.ip, Port: 9090})
		assert.Equal(t, tc.warn, strings.Contains(logs.String(), "require_compat_auth"), tc.name)
	}
}
