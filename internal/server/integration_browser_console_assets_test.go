package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every embedded module is fetched through Gin, including transitive boot imports.
func TestIntegrationBrowserConsoleEveryEmbeddedPublicAsset(t *testing.T) {
	var files []string
	require.NoError(t, fs.WalkDir(consoleAssets, "web/console", func(file string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files = append(files, file)
		}
		return err
	}))
	require.NotEmpty(t, files)
	for _, file := range []string{"sharing.mjs", "audit.mjs", "recovery.mjs"} {
		require.Contains(t, files, "web/console/"+file)
	}
	contentTypes := map[string]string{
		".html": "text/html; charset=utf-8", ".css": "text/css; charset=utf-8",
		".mjs": "text/javascript; charset=utf-8", ".js": "text/javascript; charset=utf-8",
		".svg": "image/svg+xml", ".txt": "text/plain; charset=utf-8",
	}
	s, _ := oauthTestNode(t, config.ClusterModeDisabled)
	var routes []string
	for _, file := range files {
		route := "/console/" + strings.TrimPrefix(file, "web/console/")
		if file == "web/console/index.html" {
			route = "/console/"
		}
		routes = append(routes, route)
		t.Run(route, func(t *testing.T) {
			expected, err := consoleAssets.ReadFile(file)
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, expected, rec.Body.Bytes())
			require.Contains(t, contentTypes, path.Ext(file))
			assert.Equal(t, contentTypes[path.Ext(file)], rec.Header().Get("Content-Type"))
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, "same-origin", rec.Header().Get("Referrer-Policy"))
			assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "script-src 'self'")
			assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "worker-src 'self'")
			assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
			assert.NotContains(t, rec.Header().Get("Content-Security-Policy"), "unsafe-inline")
			assert.NotContains(t, rec.Header().Get("Content-Security-Policy"), "unsafe-eval")
		})
	}
	for _, route := range []string{
		"/console/index.html", "/console/tests/", "/console/tests/workflow-completion.test.mjs",
		"/console/brand/", "/console/icons/", "/console/vendor/", "/console/vendor/package.json",
		"/console/.env", "/console/browser_console.go", "/console/unknown.mjs",
	} {
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, route)
	}
	rec := httptest.NewRecorder()
	s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console", nil))
	assert.Equal(t, http.StatusTemporaryRedirect, rec.Code)
	assert.Equal(t, "/console/", rec.Header().Get("Location"))

	crossRole(t, s, role.RoleWorker)
	withoutAuthority := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, OAuthEnabled: new(false)})
	for _, route := range routes {
		for _, server := range []*Server{s, withoutAuthority} {
			rec := httptest.NewRecorder()
			server.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
			assert.Equal(t, http.StatusNotFound, rec.Code, route)
		}
	}
}
