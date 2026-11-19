package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic replies exercise wire preservation, not live credential minting.
func TestRealtime_SessionHelpersPreserveNativeWireAndAdmission(t *testing.T) {
	upstream, hits, mu := newBackendRecorder(t, http.StatusCreated, []byte(`{"value":"synthetic-ephemeral-key","expires_at":123}`), "application/json")
	s := createTestNodeWithDefaults(t)
	cfg := &pkgConfig.AppsConfig{Version: "1.0", Name: "realtime-fixture"}
	enabled := true
	require.NoError(t, cfg.AddApp("realtime-fixture", pkgConfig.ServiceConfig{
		Enabled: &enabled, Name: "realtime-fixture", Mode: "cloud", Protocol: pkgConfig.ProtocolOpenAI,
		Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: upstream.URL, OpenAIPath: "/provider/openai", API: &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "synthetic-upstream-token"}},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	s.configStore = pkgConfig.NewMemoryAppsConfigStore(cfg)
	s.config.OpenAICompat.RealtimeBackend = "realtime-fixture"
	request := func(path, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path+"?trace=kept%2Bexact", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct{ path, body string }{
		{"/v1/realtime/client_secrets", ` { "expires_after":{"anchor":"created_at","seconds":600}, "session": {"type":"realtime","model":"native-unregistered-model","instructions":"keep me","native":9007199254740993} } `},
		{"/v1/realtime/client_secrets", `{"session":{"type":"transcription","audio":{"input":{"transcription":{"model":"whisper-1"}}}}}`},
		{"/v1/realtime/sessions", `{"model":"legacy-native-model","voice":"alloy"}`},
		{"/v1/realtime/transcription_sessions", `{"input_audio_transcription":{"model":"whisper-1"}}`},
	} {
		w := request(tc.path, TestAdminKey, tc.body)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		assert.JSONEq(t, `{"value":"synthetic-ephemeral-key","expires_at":123}`, w.Body.String())
		mu.Lock()
		got := (*hits)[len(*hits)-1]
		mu.Unlock()
		assert.Equal(t, tc.body, string(got.Body), "native body must never be reconstructed or model-rewritten")
		assert.Equal(t, strings.Replace(tc.path, "/v1/", "/provider/openai/", 1), got.Path)
		assert.Equal(t, "trace=kept%2Bexact", got.Query)
		assert.Equal(t, "Bearer synthetic-upstream-token", got.Headers.Get("Authorization"))
		assert.Empty(t, got.Headers.Get("X-API-Key"))
	}
	deny := func(key string, status int) {
		t.Helper()
		mu.Lock()
		before := len(*hits)
		mu.Unlock()
		w := request("/v1/realtime/client_secrets", key, `{"session":{"type":"realtime","model":"gpt-realtime"}}`)
		assert.Equal(t, status, w.Code, w.Body.String())
		mu.Lock()
		defer mu.Unlock()
		assert.Len(t, *hits, before, "denial must not mint an upstream credential")
	}
	deny("", http.StatusUnauthorized)
	deny("invalid", http.StatusUnauthorized)
	deny(loadTestKey(t, s, "realtime-spend", keys.VirtualKey{Name: "Spend", QuotaConfig: quota.QuotaConfig{SpendLimit: 5}}), http.StatusForbidden)
	deny(loadTestKey(t, s, "realtime-tokens", keys.VirtualKey{Name: "Tokens", QuotaConfig: quota.QuotaConfig{TPMLimit: 1000}}), http.StatusForbidden)
	team := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey, map[string]any{"id": "realtime-models", "name": "Models", "allowed_models": []string{"gpt-realtime"}})
	require.Equal(t, http.StatusCreated, team.Code, string(team.Body))
	deny(loadTestKey(t, s, "realtime-member", keys.VirtualKey{Name: "Member", TeamID: "realtime-models"}), http.StatusForbidden)
	limited := loadTestKey(t, s, "realtime-rpm", keys.VirtualKey{Name: "RPM", QuotaConfig: quota.QuotaConfig{RPMLimit: 1}})
	require.Equal(t, http.StatusCreated, request("/v1/realtime/client_secrets", limited, `{}`).Code)
	deny(limited, http.StatusTooManyRequests)
	s.config.OpenAICompat.MaxUploadBytes = 4
	deny(TestAdminKey, http.StatusRequestEntityTooLarge)
}
