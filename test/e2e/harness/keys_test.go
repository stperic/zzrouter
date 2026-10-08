package harness_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/test/e2e/harness"
)

// TestCreateVirtualKey_HappyPath exercises the typed wrapper against
// an httptest server returning the canonical SuccessResponse envelope.
func TestCreateVirtualKey_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zzrouter/v1/keys" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "admin-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"message": "Key created successfully.",
			"data": map[string]any{
				"id":   "vk_abc",
				"name": "ring2",
				"role": "user",
				"key":  "zzr-test-secret",
			},
		})
	}))
	defer srv.Close()

	node, _ := harness.NewNode("t", srv.URL, harness.RoleCoordinator, nil, harness.NodeKeys{
		Admin: "admin-key",
	})
	c := harness.NewClient(node, harness.TierAdmin)

	vk, err := harness.CreateVirtualKey(context.Background(), c, harness.VirtualKeyRequest{
		Name:     "ring2",
		RPMLimit: 5,
	})
	if err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if vk.ID != "vk_abc" || vk.RawKey != "zzr-test-secret" {
		t.Errorf("vk=%+v", vk)
	}
}

// TestCreateVirtualKey_RejectsMissingName pins input validation —
// the server requires name; the helper short-circuits before the
// HTTP call to give the test a cleaner error.
func TestCreateVirtualKey_RejectsMissingName(t *testing.T) {
	c := harness.NewClient(mustNode(t, "http://example.invalid"), harness.TierAdmin)
	_, err := harness.CreateVirtualKey(context.Background(), c, harness.VirtualKeyRequest{})
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected name-required error, got %v", err)
	}
}

// TestDeleteVirtualKey_404IsSuccess pins the idempotency contract
// on cleanup — re-running a flaky test shouldn't fail because the
// previous run already removed the key.
func TestDeleteVirtualKey_404IsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	node, _ := harness.NewNode("t", srv.URL, harness.RoleCoordinator, nil, harness.NodeKeys{
		Admin: "admin-key",
	})
	c := harness.NewClient(node, harness.TierAdmin)
	if err := harness.DeleteVirtualKey(context.Background(), c, "vk_x"); err != nil {
		t.Errorf("404 should be treated as success: %v", err)
	}
}

func mustNode(t *testing.T, url string) *harness.Node {
	t.Helper()
	n, err := harness.NewNode("t", url, harness.RoleCoordinator, nil, harness.NodeKeys{Admin: "admin-key"})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return n
}
