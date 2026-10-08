package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
)

// CORSMiddleware enforces the CORS policy declared in cfg. Disabled
// configs return a zero-cost passthrough. Requests without an Origin
// header bypass CORS entirely (curl, server-to-server).
//
// Origins not in the allowed list are not blocked here — we forward
// the request with no Access-Control-Allow-Origin header so the browser
// refuses the response. That avoids leaking whether a specific origin
// is configured.
func CORSMiddleware(cfg config.CORSConfig) gin.HandlerFunc {
	if !cfg.Enabled {
		return func(c *gin.Context) { c.Next() }
	}

	// Defaults scoped inside the constructor so other code in this
	// package cannot mutate them. Picked to cover the routes a browser
	// would realistically hit: inference (POST /v1/*, /api/*) and
	// management (GET /zzrouter/v1/*). OPTIONS is always allowed
	// because it is the preflight verb itself.
	methods := cfg.AllowedMethods
	if len(methods) == 0 {
		methods = []string{http.MethodGet, http.MethodPost, http.MethodOptions}
	}
	headers := cfg.AllowedHeaders
	if len(headers) == 0 {
		headers = []string{"X-API-Key", "Content-Type", "Authorization"}
	}
	exposed := cfg.ExposedHeaders
	if len(exposed) == 0 {
		exposed = []string{"X-Request-ID", "X-API-Version"}
	}
	maxAge := cfg.MaxAgeSeconds
	if maxAge == 0 {
		maxAge = 600
	}

	methodsHeader := strings.Join(methods, ", ")
	headersHeader := strings.Join(headers, ", ")
	exposedHeader := strings.Join(exposed, ", ")
	maxAgeHeader := strconv.Itoa(maxAge)

	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	wildcard := false
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			wildcard = true
			continue
		}
		allowed[o] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}

		// Vary: Origin is set before any allow/deny decision because
		// the decision itself varies by Origin. Shared caches (CDN,
		// Varnish) will otherwise key without Origin and can serve a
		// denied-origin (no-ACAO) response to a later allowed-origin
		// request, silently breaking CORS for that origin.
		h := c.Writer.Header()
		h.Add("Vary", "Origin")

		allowOrigin := ""
		switch {
		case wildcard:
			allowOrigin = "*"
		default:
			if _, ok := allowed[origin]; ok {
				allowOrigin = origin
			}
		}

		if allowOrigin == "" {
			c.Next()
			return
		}

		h.Set("Access-Control-Allow-Origin", allowOrigin)
		if cfg.AllowCredentials {
			h.Set("Access-Control-Allow-Credentials", "true")
		}

		if c.Request.Method == http.MethodOptions && c.GetHeader("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", methodsHeader)
			h.Set("Access-Control-Allow-Headers", headersHeader)
			h.Set("Access-Control-Max-Age", maxAgeHeader)
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		h.Set("Access-Control-Expose-Headers", exposedHeader)
		c.Next()
	}
}
