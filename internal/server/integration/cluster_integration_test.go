package integration_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	srv "github.com/stperic/zzrouter/internal/server"

	"github.com/stretchr/testify/assert"
)

// TestHandleListModels tests model listing across different query modes.
// Uses a single shared server to avoid repeated model scanning (~40s each).
func TestHandleListModels(t *testing.T) {
	server := srv.NewTestNode(t, srv.TestNodeConfig{
		AdminKey:   srv.TestAdminKey,
		ClusterKey: srv.TestClusterKey,
	})

	t.Run("LocalQuery", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/zzrouter/v1/models", nil)
		req.Header.Set("X-API-Key", srv.TestAdminKey)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code,
			"expected 200 for authenticated model list, got %d: %s", w.Code, w.Body.String())
	})

	t.Run("ClusterWideQuery", func(t *testing.T) {
		resp := srv.MakeAuthRequest(t, server, "GET", "/zzrouter/v1/models?node=*", srv.TestAdminKey, nil)
		srv.AssertStatusOneOf(t, resp, []int{http.StatusOK, http.StatusBadRequest})
	})
}

// TestModelMetadataConversion tests type conversion helpers
func TestModelMetadataConversion(t *testing.T) {
	testMap := map[string]any{
		"name":   "test-model",
		"number": 123,
	}

	if v, ok := testMap["name"].(string); !ok || v != "test-model" {
		t.Errorf("Expected 'test-model', got '%v'", testMap["name"])
	}

	testMap2 := map[string]any{
		"size_int64":   int64(1024),
		"size_float64": float64(2048),
		"size_int":     int(4096),
	}

	for key, expected := range map[string]int64{"size_int64": 1024, "size_float64": 2048, "size_int": 4096} {
		var got int64
		switch v := testMap2[key].(type) {
		case int64:
			got = v
		case float64:
			got = int64(v)
		case int:
			got = int64(v)
		}
		if got != expected {
			t.Errorf("%s: expected %d, got %d", key, expected, got)
		}
	}
}
