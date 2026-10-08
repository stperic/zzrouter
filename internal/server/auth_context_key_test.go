package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// TestInternalRequestKey_UnspoofableAcrossPackages asserts that the
// trusted-internal-request bypass can only fire when the context value
// was set under the real unexported internalRequestKey — not a foreign
// type with the same name or shape. context.Value compares keys by
// identity AND type, so only in-package callers can trigger the bypass.
func TestInternalRequestKey_UnspoofableAcrossPackages(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := createTestNodeWithDefaults(t)
	middleware := server.auth.AuthMiddleware(RoleAdmin)

	// Attacker vector 1: a caller sets a context value under a typed
	// string key whose value matches the real key's variable name.
	// Go's context.Value compares keys by type AND value, so a string-
	// backed key never collides with internalRequestKeyType.
	t.Run("typed string key with same value does not bypass", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		req := httptest.NewRequest("GET", "/zzrouter/v1/models", nil)
		var attackerKey stringKey = "internalRequestKey"
		req = req.WithContext(context.WithValue(req.Context(), attackerKey, true))
		c.Request = req

		middleware(c)

		assert.Equal(t, http.StatusUnauthorized, w.Code,
			"bypass must not fire: typed-string key must not alias internalRequestKey")
	})

	// Attacker vector 2: a caller that constructs a foreign struct{} type
	// with the same shape as internalRequestKeyType and sets the value.
	// Different type identity → different context key → no bypass.
	t.Run("foreign struct{} key does not bypass", func(t *testing.T) {
		type attackerKeyType struct{}
		var attackerKey attackerKeyType

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		req := httptest.NewRequest("GET", "/zzrouter/v1/models", nil)
		req = req.WithContext(context.WithValue(req.Context(), attackerKey, true))
		c.Request = req

		middleware(c)

		assert.Equal(t, http.StatusUnauthorized, w.Code,
			"bypass must not fire: foreign struct{} key must not alias internalRequestKey")
	})

	// Attacker vector 3: missing value entirely (control). Proves the
	// request would have been denied without any bypass tampering.
	t.Run("no context value denies normally", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/zzrouter/v1/models", nil)

		middleware(c)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	// Positive control: a value set under the real internalRequestKey from
	// within the package DOES trigger the bypass. If this fails, the
	// in-package trusted-dispatch path is broken — which is what
	// ServeClusterRequest relies on for in-process routing.
	t.Run("in-package internalRequestKey triggers bypass", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		req := httptest.NewRequest("GET", "/zzrouter/v1/models", nil)
		req = req.WithContext(context.WithValue(req.Context(), internalRequestKey, true))
		c.Request = req

		middleware(c)

		assert.NotEqual(t, http.StatusUnauthorized, w.Code,
			"in-package internalRequestKey must still grant the bypass — this is the trusted dispatch path")
	})
}

// stringKey is a distinct type used by the attacker-vector-1 subtest to
// prove that type identity (not just underlying value) gates context keys.
type stringKey string
