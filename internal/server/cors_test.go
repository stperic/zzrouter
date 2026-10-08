package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCORSEngine(cfg config.CORSConfig) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(CORSMiddleware(cfg))
	e.GET("/probe", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	e.POST("/probe", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return e
}

func TestCORS_Disabled_IsZeroOp(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{Enabled: false})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, w.Header().Get("Vary"))
}

func TestCORS_NoOriginHeader_IsZeroOp(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_OriginNotAllowed_ForwardsWithoutHeaders(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Origin", "https://attacker.example.com")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "ok", w.Body.String())

	// Cache-safety: the response MUST still Vary on Origin even though
	// we emitted no CORS headers. Otherwise shared caches can serve
	// this denied-origin response to a later allowed-origin request.
	assert.Contains(t, w.Header().Values("Vary"), "Origin")
}

// Pins the Vary: Origin contract on the allowed path too — it's the
// same cache-safety invariant expressed from the other side.
func TestCORS_AllowedOrigin_SetsVaryOrigin(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
	})
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Contains(t, w.Header().Values("Vary"), "Origin")
}

// Preexisting Vary values (e.g. from a compression middleware) must be
// preserved — CORS appends, never overwrites.
func TestCORS_PreservesExistingVary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(func(c *gin.Context) {
		c.Writer.Header().Add("Vary", "Accept-Encoding")
		c.Next()
	})
	e.Use(CORSMiddleware(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
	}))
	e.GET("/probe", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	vary := w.Header().Values("Vary")
	assert.Contains(t, vary, "Accept-Encoding")
	assert.Contains(t, vary, "Origin")
}

// OPTIONS without Access-Control-Request-Method is NOT a preflight —
// treat as a normal cross-origin request (emit ACAO + Expose-Headers,
// no 204, handler runs).
func TestCORS_OptionsWithoutACRM_NotPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(CORSMiddleware(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
	}))
	e.OPTIONS("/probe", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodOptions, "/probe", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "https://dashboard.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "X-Request-ID, X-API-Version", w.Header().Get("Access-Control-Expose-Headers"))
}

func TestCORS_NormalCrossOrigin_EmitsHeaders(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "https://dashboard.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, w.Header().Values("Vary"), "Origin")
	assert.Equal(t, "X-Request-ID, X-API-Version", w.Header().Get("Access-Control-Expose-Headers"))
}

func TestCORS_PreflightHappyPath(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
	})

	req := httptest.NewRequest(http.MethodOptions, "/probe", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "X-API-Key, Content-Type")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "https://dashboard.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, POST, OPTIONS", w.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "X-API-Key, Content-Type, Authorization", w.Header().Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "600", w.Header().Get("Access-Control-Max-Age"))

	vary := strings.Join(w.Header().Values("Vary"), ", ")
	assert.Contains(t, vary, "Origin")
	assert.Contains(t, vary, "Access-Control-Request-Method")
	assert.Contains(t, vary, "Access-Control-Request-Headers")
}

func TestCORS_PreflightDeniedOrigin_NoHeaders(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
	})

	req := httptest.NewRequest(http.MethodOptions, "/probe", nil)
	req.Header.Set("Origin", "https://attacker.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	// No allow-origin emitted; the route handler runs and returns its
	// normal response. The browser will reject it.
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_Credentials_EmitsCredentialsHeader(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{
		Enabled:          true,
		AllowedOrigins:   []string{"https://dashboard.example.com"},
		AllowCredentials: true,
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, "https://dashboard.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", w.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, w.Header().Values("Vary"), "Origin")
}

func TestCORS_WildcardWithoutCredentials_EmitsStar(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"*"},
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Origin", "https://anything.example.com")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"))
}

func TestCORS_CustomAllowAndExposeHeaders(t *testing.T) {
	e := newCORSEngine(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
		AllowedMethods: []string{"GET", "DELETE"},
		AllowedHeaders: []string{"X-Custom"},
		ExposedHeaders: []string{"X-Trace-ID"},
		MaxAgeSeconds:  120,
	})

	req := httptest.NewRequest(http.MethodOptions, "/probe", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	req.Header.Set("Access-Control-Request-Method", "DELETE")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "GET, DELETE", w.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "X-Custom", w.Header().Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "120", w.Header().Get("Access-Control-Max-Age"))

	// On the follow-up non-preflight request, Expose-Headers reflects
	// the custom list.
	req2 := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req2.Header.Set("Origin", "https://dashboard.example.com")
	w2 := httptest.NewRecorder()
	e.ServeHTTP(w2, req2)
	assert.Equal(t, "X-Trace-ID", w2.Header().Get("Access-Control-Expose-Headers"))
}

func TestCORSConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.CORSConfig
		wantErr string
	}{
		{
			name: "disabled_passes_even_with_bad_settings",
			cfg: config.CORSConfig{
				Enabled:          false,
				AllowedOrigins:   []string{"*"},
				AllowCredentials: true,
				MaxAgeSeconds:    -1,
			},
		},
		{
			name:    "enabled_requires_origins",
			cfg:     config.CORSConfig{Enabled: true},
			wantErr: "allowed_origins",
		},
		{
			name: "wildcard_with_credentials_rejected",
			cfg: config.CORSConfig{
				Enabled:          true,
				AllowedOrigins:   []string{"*"},
				AllowCredentials: true,
			},
			wantErr: "allow_credentials",
		},
		{
			name: "negative_max_age_rejected",
			cfg: config.CORSConfig{
				Enabled:        true,
				AllowedOrigins: []string{"https://x.example"},
				MaxAgeSeconds:  -5,
			},
			wantErr: "max_age_seconds",
		},
		{
			name: "ok_specific_origin_with_credentials",
			cfg: config.CORSConfig{
				Enabled:          true,
				AllowedOrigins:   []string{"https://x.example"},
				AllowCredentials: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestCORS_ClusterModeGate_PreflightNotBlocked(t *testing.T) {
	// Asserts the documented invariant: a preflight to a ClusterModeGate-
	// guarded route must 204 with CORS headers, not 503. CORS runs before
	// the gate, and the preflight aborts before the gate ever sees the
	// request.
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(CORSMiddleware(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://dashboard.example.com"},
	}))
	e.Use(func(c *gin.Context) {
		// Stand-in for ClusterModeGate: unconditionally 503.
		c.AbortWithStatus(http.StatusServiceUnavailable)
	})
	e.POST("/guarded", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodOptions, "/guarded", nil)
	req.Header.Set("Origin", "https://dashboard.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "https://dashboard.example.com", w.Header().Get("Access-Control-Allow-Origin"))
}
