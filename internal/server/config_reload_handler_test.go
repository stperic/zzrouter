package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigReloadHandler_MountedAndAuthenticated verifies three contracts
// in one round trip: the route is registered (no 404), admin auth is
// enforced (no-key → 401), and the handler gracefully handles a nil
// configStore (503 instead of panic). The test harness creates a node
// with no providers/ dir, so configStore is nil — which is the real
// failure mode we want to guard.
func TestConfigReloadHandler_MountedAndAuthenticated(t *testing.T) {
	srv := createTestNodeWithDefaults(t)
	httpNode := httptest.NewServer(srv.engine)
	defer closeHTTPTestServer(httpNode)

	t.Run("no_auth_401", func(t *testing.T) {
		req, err := http.NewRequest("POST", httpNode.URL+"/zzrouter/v1/config/reload", strings.NewReader(""))
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("authed_disabled_feature_503", func(t *testing.T) {
		req, err := http.NewRequest("POST", httpNode.URL+"/zzrouter/v1/config/reload", nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", TestAdminKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
			"missing provider config should 503, not 404 or 500")
	})
}
