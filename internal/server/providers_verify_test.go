package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

type recordingRegistrar struct{ finalized []string }

func (r *recordingRegistrar) FinalizeOnboarding(name string) error {
	r.finalized = append(r.finalized, name)
	return nil
}

func (r *recordingRegistrar) FinalizeOffboarding(string) error { return nil }

// Verifying a provider, which runs before it is enabled, sends the
// credential it declares, so a token-protected endpoint verifies.
func TestVerifyProvider_SendsTheDeclaredCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cloud-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	cfg := &pkgConfig.AppsConfig{Version: "1.0", Name: "test"}
	require.NoError(t, cfg.AddApp("cloudy", pkgConfig.ServiceConfig{
		Enabled: new(false), Name: "Cloudy", Protocol: pkgConfig.ProtocolOpenAI, Mode: "cloud",
		Runtime: &pkgConfig.AppRuntimeConfig{
			Endpoint: upstream.URL,
			API:      &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "cloud-token"},
		},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	reg := &recordingRegistrar{}
	ctrl := &ProvidersController{appsConfig: func() *pkgConfig.AppsConfig { return cfg }, registrar: reg}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/zzrouter/v1/providers/:name/verify", ctrl.VerifyProvider)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/zzrouter/v1/providers/cloudy/verify", nil))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"cloudy"}, reg.finalized, "verify reached the endpoint and was accepted")
}
