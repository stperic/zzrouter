package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed web/console/*.html web/console/*.css web/console/*.mjs web/console/brand/*.svg web/console/icons/*.svg web/console/vendor/*.mjs web/console/vendor/*.js web/console/vendor/*.txt
var consoleAssets embed.FS

// registerBrowserConsole exposes a fixed public shell; the existing API owns
// every identity, permission, provider operation and protected value.
func (s *Server) registerBrowserConsole() {
	if s.access.oauth == nil {
		return
	}
	console := s.engine.Group("/console", browserResponseHeaders, func(c *gin.Context) {
		c.Header("Content-Security-Policy", "default-src 'none'; script-src 'self'; worker-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
		c.Next()
	}, s.browserCoordinatorOnly)
	console.GET("", func(c *gin.Context) { c.Redirect(http.StatusTemporaryRedirect, "/console/") })
	contentTypes := map[string]string{
		".html": "text/html; charset=utf-8", ".css": "text/css; charset=utf-8",
		".mjs": "text/javascript; charset=utf-8", ".js": "text/javascript; charset=utf-8",
		".svg": "image/svg+xml", ".txt": "text/plain; charset=utf-8",
	}
	err := fs.WalkDir(consoleAssets, "web/console", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		route := strings.TrimPrefix(file, "web/console")
		if route == "/index.html" {
			route = "/"
		}
		body, err := consoleAssets.ReadFile(file)
		if err != nil {
			return err
		}
		contentType := contentTypes[path.Ext(file)]
		console.GET(route, func(c *gin.Context) { c.Data(http.StatusOK, contentType, body) })
		return nil
	})
	if err != nil {
		panic("invalid embedded console assets: " + err.Error())
	}
}
