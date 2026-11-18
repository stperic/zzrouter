package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A client generated from the spec sends a key only where the spec asks
// for one. Every inference operation must say it takes a key, in either
// header. Native management mutations always require an administrator;
// inference may be configured without a key. Only the two console
// inference operations also accept a browser session.
func TestOpenAPI_InferenceRoutesDeclareTheirKey(t *testing.T) {
	paths, ok := mapAt(loadSpecTree(t), "paths")
	require.True(t, ok)
	checked, browserOperations := 0, 0
	for path, item := range paths {
		if !strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/api/") {
			continue
		}
		ops, _ := item.(map[string]any)
		for method, op := range ops {
			opMap, ok := op.(map[string]any)
			if !ok || opMap["responses"] == nil {
				continue
			}
			want := []any{
				map[string]any{"InferenceBearer": []any{}},
				map[string]any{"InferenceKey": []any{}},
			}
			if path == "/v1/models" && method == "get" || path == "/v1/chat/completions" && method == "post" {
				want = append(want, map[string]any{"BrowserSession": []any{}})
				browserOperations++
			}
			switch rk(method, path) {
			case "POST /api/pull", "POST /api/create", "DELETE /api/delete", "POST /api/copy", "POST /api/push", "POST /api/blobs/{digest}":
				assert.Contains(t, opMap["description"], "administrator key")
			case "GET /v1/responses/{response_id}", "DELETE /v1/responses/{response_id}", "POST /v1/responses/{response_id}/cancel", "GET /v1/responses/{response_id}/input_items":
				assert.Contains(t, opMap["description"], "authenticated owner")
			default:
				want = append(want, map[string]any{})
			}
			assert.Equalf(t, want, opMap["security"], "%s %s", strings.ToUpper(method), path)
			checked++
		}
	}
	assert.Positive(t, checked)
	assert.Equal(t, 2, browserOperations)
}
