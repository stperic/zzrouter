package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stretchr/testify/require"
)

func TestConnectionStoredUsesBoundSnapshotWithoutActivation(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "external", true: "managed"}[managed], func(t *testing.T) {
			var auth, request atomic.Value
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth.Store(r.Header.Get("Authorization"))
				request.Store(r.Method + " " + r.URL.Path)
				_, _ = io.WriteString(w, `{"secret":"upstream-body-must-not-leak"}`)
			}))
			defer upstream.Close()
			h := newConnectionHarness(t, upstream.URL)
			token := "external-connection-fixture"
			if managed {
				token = "stored-connection-fixture"
				_, err := h.service.Save(t.Context(), "fixture", 0, map[string]string{"endpoint": upstream.URL, "token": token}, false)
				require.NoError(t, err)
			}
			before, err := h.service.Status("fixture")
			require.NoError(t, err)
			require.False(t, before.Enabled, "testing must also work before activation")
			published := h.published
			rec := httptest.NewRecorder()
			h.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/zzrouter/v1/providers/fixture/connection/test", strings.NewReader(`{"stored":true}`)))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			require.Contains(t, rec.Body.String(), `"code":"accepted"`)
			require.Equal(t, "Bearer "+token, auth.Load())
			require.Equal(t, "GET /v1/models", request.Load())
			require.NotContains(t, rec.Body.String(), token)
			require.NotContains(t, rec.Body.String(), "upstream-body-must-not-leak")
			after, err := h.service.Status("fixture")
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Zero(t, h.registrar.calls)
			require.Equal(t, published, h.published)
		})
	}
}

func TestConnectionStoredRefusesAmbiguousOrUnavailableRequests(t *testing.T) {
	for _, tc := range []struct {
		name, body, suffix, code string
		status                   int
		prepare                  func(*testing.T, *connectionHarness)
	}{
		{name: "missing mode", body: `{}`, status: 400},
		{name: "null candidate", body: `{"values":null}`, status: 400},
		{name: "non-string candidate", body: `{"values":{"token":42}}`, status: 400},
		{name: "mixed mode", body: `{"stored":true,"values":{"token":"secret-fixture"}}`, status: 400},
		{name: "mixed null", body: `{"stored":true,"values":null}`, status: 400},
		{name: "node selection", body: `{"stored":true}`, suffix: "?node=worker", status: 400},
		{name: "candidate never inherits secrets", body: `{"values":{"endpoint":"http://127.0.0.1:1"}}`, code: "invalid_provider_connection", status: 400},
		{name: "missing credentials", body: `{"stored":true}`, code: "provider_connection_not_configured", status: 400, prepare: func(t *testing.T, _ *connectionHarness) { t.Setenv("CONNECTION_TEST_TOKEN", "") }},
		{name: "removed credentials", body: `{"stored":true}`, code: "provider_connection_not_configured", status: 400, prepare: func(t *testing.T, h *connectionHarness) {
			_, err := h.vault.Remove(t.Context(), "fixture", 0)
			require.NoError(t, err)
		}},
		{name: "closed storage", body: `{"stored":true}`, code: "provider_connection_unavailable", status: 503, prepare: func(t *testing.T, h *connectionHarness) { require.NoError(t, h.vault.Close()) }},
		{name: "wrong coordinator", body: `{"stored":true}`, code: "provider_connection_scope", status: 409, prepare: func(_ *testing.T, h *connectionHarness) { h.service.coordinator = func() bool { return false } }},
		{name: "availability changes", body: `{"stored":true}`, code: "provider_connection_unavailable", status: 503, prepare: func(_ *testing.T, h *connectionHarness) {
			reads := 0
			h.service.resolver = backend.NewResolver(func() *config.AppsConfig {
				reads++
				if reads > 1 {
					return nil
				}
				return h.config.Config()
			}, h.vault)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
			defer upstream.Close()
			h := newConnectionHarness(t, upstream.URL)
			if tc.prepare != nil {
				tc.prepare(t, h)
			}
			rec := httptest.NewRecorder()
			h.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/zzrouter/v1/providers/fixture/connection/test"+tc.suffix, strings.NewReader(tc.body)))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			if tc.code != "" {
				require.Contains(t, rec.Body.String(), `"code":"`+tc.code+`"`)
			}
			require.NotContains(t, rec.Body.String(), "secret-fixture")
			require.Zero(t, calls.Load())
			require.Zero(t, h.registrar.calls)
			require.Zero(t, h.published)
		})
	}
}

func TestConnectionStoredProbeRefusesRedirectAndRedactsFailure(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { destinationCalls.Add(1); w.WriteHeader(200) }))
	defer destination.Close()
	for _, status := range []int{302, 401, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, r.Header.Get("Authorization")+" private upstream detail")
			}))
			defer upstream.Close()
			h := newConnectionHarness(t, upstream.URL)
			result, err := h.service.TestStored(t.Context(), "fixture")
			require.NoError(t, err)
			require.Equal(t, "failed", result.Status)
			require.Equal(t, status, result.HTTPStatus)
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "external-connection-fixture")
			require.NotContains(t, string(encoded), "private upstream detail")
		})
	}
	require.Zero(t, destinationCalls.Load())
}

func TestIntegrationBrowserStoredConnectionUsesExistingAuthority(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	h := newConnectionHarness(t, upstream.URL)
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	// Keep the real server's controller, browser policy and coordinator gate;
	// bind only its connection lookup to isolated synthetic credentials.
	s.providers.connections.resolver = h.service.resolver
	path := "/zzrouter/v1/providers/fixture/connection/test"
	body := map[string]bool{"stored": true}
	for _, auth := range []struct {
		key    string
		status int
	}{{"", 401}, {TestUserKey, 403}} {
		result := makeAuthRequest(t, s, http.MethodPost, path, auth.key, body)
		require.Equal(t, auth.status, result.Code, string(result.Body))
	}
	admin := newSessionBrowser(t, endpoint, TestAdminKey)
	for _, headers := range []map[string]string{{"X-CSRF-Token": ""}, {"X-CSRF-Token": "wrong-proof"}, {"Origin": "https://other.invalid"}} {
		status, _ := admin.call(t, http.MethodPost, path, body, headers)
		require.Equal(t, http.StatusForbidden, status)
	}
	user := newSessionBrowser(t, endpoint, TestUserKey)
	status, _ := user.call(t, http.MethodPost, path, body, nil)
	require.Equal(t, http.StatusForbidden, status)
	require.Zero(t, requests.Load(), "refused callers must not send stored credentials upstream")
	status, response := admin.call(t, http.MethodPost, path, body, nil)
	require.Equal(t, http.StatusOK, status, string(response))
	require.Contains(t, string(response), `"code":"accepted"`)
	result := makeAuthRequest(t, s, http.MethodPost, path, TestAdminKey, body)
	require.Equal(t, http.StatusOK, result.Code, string(result.Body))
	require.EqualValues(t, 2, requests.Load())
}

func TestConnectionStoredWithoutProbeReportsUnknown(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	h := newConnectionHarness(t, upstream.URL)
	require.NoError(t, h.config.ApplyParameterPatch("fixture", func(provider *config.ServiceConfig) error { provider.Runtime.ModelsPath = new(""); return nil }))
	result, err := h.service.TestStored(t.Context(), "fixture")
	require.NoError(t, err)
	require.Equal(t, "unknown", result.Status)
	require.Equal(t, "probe_unavailable", result.Code)
	require.Zero(t, calls.Load(), "missing provider probe must not invent a network request")
}
