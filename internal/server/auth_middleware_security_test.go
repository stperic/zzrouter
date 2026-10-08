package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// TestXInternalRequestHeader_NotTrustedFromExternal verifies that an external client
// sending the X-Internal-Request header cannot bypass authentication.
// This is a CRITICAL security test — the header was previously trusted directly.
func TestXInternalRequestHeader_NotTrustedFromExternal(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	gin.SetMode(gin.TestMode)
	middleware := server.auth.AuthMiddleware(RoleAdmin)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/zzrouter/v1/models", nil)
	// An external attacker sets this header — it must NOT grant access
	c.Request.Header.Set("X-Internal-Request", "true")

	middleware(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"SECURITY: X-Internal-Request header from external client must not bypass auth")
}
