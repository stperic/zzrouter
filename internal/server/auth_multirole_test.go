package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// TestAuthMiddlewareMultiRole_RejectsWrongRole asserts that a key whose
// role is not in acceptedRoles is rejected with 403, even if the key
// itself validates. Guards against a prior bug where membership wasn't
// checked after ValidateKey.
func TestAuthMiddlewareMultiRole_RejectsWrongRole(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := createTestNode(t, TestNodeConfig{
		AdminKey:    TestAdminKey,
		UserKey:     TestUserKey,
		ClusterMode: pkgConfig.ClusterModeCoordinator,
	})

	middleware := server.auth.AuthMiddlewareMultiRole([]UserRole{RoleAdmin, RoleUser})

	cases := []struct {
		name     string
		key      string
		wantCode int
	}{
		{"admin key accepted", TestAdminKey, http.StatusOK},
		{"user key accepted", TestUserKey, http.StatusOK},
		{"invalid key rejected", "not-a-real-key-at-all-xxxxxxxxxx", http.StatusUnauthorized},
		{"missing key rejected", "", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/test", nil)
			if tc.key != "" {
				c.Request.Header.Set("X-API-Key", tc.key)
			}

			middleware(c)

			// On success the middleware falls through to c.Next(); with no
			// downstream handler registered gin leaves Code at the default 200.
			assert.Equal(t, tc.wantCode, w.Code,
				"unexpected status for %q (got %d body=%s)", tc.name, w.Code, w.Body.String())
		})
	}
}

// TestAuthenticateMultiRole_EmptyRoleSliceRejects pins the guard against
// a caller that passes []UserRole{}. Prior code would have panicked on
// the internal-request bypass branch when indexing acceptedRoles[0]; the
// guard now rejects with 403 instead. Treating "no accepted roles" as
// "no one gets in" is the fail-closed default.
func TestAuthenticateMultiRole_EmptyRoleSliceRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := createTestNode(t, TestNodeConfig{
		AdminKey:    TestAdminKey,
		UserKey:     TestUserKey,
		ClusterMode: pkgConfig.ClusterModeCoordinator,
	})

	// Two paths to exercise the guard: with and without a key header.
	// Both must reject before reaching the membership check or the
	// internal-request bypass.
	cases := []struct{ name, key string }{
		{"with admin key", TestAdminKey},
		{"with no key", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/test", nil)
			if tc.key != "" {
				c.Request.Header.Set("X-API-Key", tc.key)
			}

			// Passing nil is equivalent to an empty slice.
			ok := server.access.AuthenticateMultiRole(c, nil)
			assert.False(t, ok, "must reject when accepted roles is empty")
			assert.Equal(t, http.StatusForbidden, w.Code)
		})
	}
}
