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
		Runtime:   &config.AppRuntimeConfig{API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "t"}},
	}
	v, ok := ProbeOllama(context.Background(), svc)
	assert.True(t, ok)
	assert.Equal(t, "0.12.0", v)

	svc.Runtime.API = nil
	_, ok = ProbeOllama(context.Background(), svc)
	assert.False(t, ok)
}
