package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

func providerStatusRouter(t *testing.T, providers int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &pkgConfig.AppsConfig{}
	enabled := true
	for i := range providers {
		require.NoError(t, cfg.AddApp(fmt.Sprintf("engine%02d", i), pkgConfig.ServiceConfig{
			Enabled:      &enabled,
			Protocol:     pkgConfig.ProtocolOllama,
			Mode:         constants.AppModeExternal,
			Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: "http://127.0.0.1:11434"},
			Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		}))
	}

	ctrl := &ProviderStatusController{appsConfig: func() *pkgConfig.AppsConfig { return cfg }}
	r := gin.New()
	r.GET("/zzrouter/v1/providers/status", ctrl.ListProviderStatus)
	return r
}

func getProviderStatus(t *testing.T, r *gin.Engine, query string) (*httptest.ResponseRecorder, ListResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/zzrouter/v1/providers/status"+query, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var body ListResponse
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	}
	return w, body
}

// The endpoint accepted ?limit and returned the full set anyway, so a
// caller paging through providers had no way to tell it had been
// ignored. Every other list endpoint routes through ParsePagination.
func TestProviderStatusHonorsLimit(t *testing.T) {
	r := providerStatusRouter(t, 6)

	_, all := getProviderStatus(t, r, "")
	require.Equal(t, 6, all.Total)
	assert.Len(t, all.Data, 6)
	assert.False(t, all.HasMore)

	_, page := getProviderStatus(t, r, "?limit=2")
	assert.Len(t, page.Data, 2, "limit must actually truncate the page")
	assert.Equal(t, 6, page.Total, "total still reports the full set")
	assert.True(t, page.HasMore, "a truncated page must say so")
}

// Offset has to move the window, or limit alone cannot page.
func TestProviderStatusHonorsOffset(t *testing.T) {
	r := providerStatusRouter(t, 6)

	_, first := getProviderStatus(t, r, "?limit=2")
	_, second := getProviderStatus(t, r, "?limit=2&offset=2")

	require.Len(t, first.Data, 2)
	require.Len(t, second.Data, 2)
	assert.NotEqual(t, first.Data, second.Data, "offset must move the window")

	_, past := getProviderStatus(t, r, "?offset=99")
	assert.Empty(t, past.Data)
	assert.False(t, past.HasMore)
}

// A malformed limit is a client error, not something to silently treat
// as "give me everything".
func TestProviderStatusRejectsBadLimit(t *testing.T) {
	r := providerStatusRouter(t, 3)
	for _, q := range []string{"?limit=abc", "?limit=0", "?limit=-1", "?offset=-1"} {
		t.Run(q, func(t *testing.T) {
			w, _ := getProviderStatus(t, r, q)
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}
