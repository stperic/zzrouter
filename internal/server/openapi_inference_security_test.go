package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A client generated from the spec sends a key only where the spec asks
// for one. Every inference operation must say it takes a key, in either
// header, and that a node may be set to answer without one. Only the two
// console inference operations also accept a browser session.
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
			want = append(want, map[string]any{})
			assert.Equalf(t, want, opMap["security"], "%s %s", strings.ToUpper(method), path)
			checked++
		}
	}
	assert.Positive(t, checked)
	assert.Equal(t, 2, browserOperations)
}
