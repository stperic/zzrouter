package detect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stperic/zzrouter/pkg/config"
)

// A daemon behind auth answers the probe when it carries the credential
// its provider declares, and reads as absent without it.
func TestProbeOllama_SendsTheDeclaredCredential(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"version":"0.12.0"}`))
	}))
	defer daemon.Close()

	svc := &config.ServiceConfig{
		Discovery: &config.AppDiscovery{Detection: []config.AppDetectionRule{{Method: "http", Target: daemon.URL + "/api/version"}}},
		Runtime:   &config.AppRuntimeConfig{Endpoint: daemon.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "t"}},
	}
	v, ok := ProbeOllama(context.Background(), svc)
	assert.True(t, ok)
	assert.Equal(t, "0.12.0", v)

	svc.Runtime.API = nil
	_, ok = ProbeOllama(context.Background(), svc)
	assert.False(t, ok)
}

func TestOllamaProbeKeepsEndpointPrefixExactlyOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/ollama/api/version?format=json", r.URL.RequestURI())
		assert.Equal(t, "Bearer fixture-credential", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"version":"0.12.0"}`))
	}))
	defer upstream.Close()
	service := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Endpoint: upstream.URL + "/ollama",
			API:      &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "fixture-credential"}, //nolint:gosec // Synthetic credential used only by this test server.
		},
		Discovery: &config.AppDiscovery{Detection: []config.AppDetectionRule{{Method: "http", Target: upstream.URL + "/ollama/api/version?format=json"}}},
	}
	version, ok := ProbeOllama(t.Context(), service)
	assert.True(t, ok)
	assert.Equal(t, "0.12.0", version)
}
