package server

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed web/console/*.html web/console/*.css web/console/*.mjs
var consoleAssets embed.FS

// registerBrowserConsole exposes a fixed public shell; the existing API owns
// every identity, permission, provider operation and protected value.
func (s *Server) registerBrowserConsole() {
	if s.access.oauth == nil {
		return
	}
	console := s.engine.Group("/console", browserResponseHeaders, func(c *gin.Context) {
		c.Header("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
		c.Next()
	}, s.browserCoordinatorOnly)
	console.GET("", func(c *gin.Context) { c.Redirect(http.StatusTemporaryRedirect, "/console/") })
	for _, asset := range []struct{ path, file, contentType string }{
		{"/", "index.html", "text/html; charset=utf-8"},
		{"/console.css", "console.css", "text/css; charset=utf-8"},
		{"/console.mjs", "console.mjs", "text/javascript; charset=utf-8"},
		{"/api.mjs", "api.mjs", "text/javascript; charset=utf-8"},
		{"/dom.mjs", "dom.mjs", "text/javascript; charset=utf-8"},
		{"/providers.mjs", "providers.mjs", "text/javascript; charset=utf-8"},
		{"/access.mjs", "access.mjs", "text/javascript; charset=utf-8"},
	} {
		body, err := consoleAssets.ReadFile("web/console/" + asset.file)
		if err != nil {
			panic("missing embedded console asset: " + asset.file)
		}
		console.GET(asset.path, func(c *gin.Context) { c.Data(http.StatusOK, asset.contentType, body) })
	}
}
