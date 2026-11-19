package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The user namespace is separate from both compatibility APIs and management.
// Keep its live inventory, schema and strict audience in agreement.
func TestOpenAPIDrift_ProviderMetadataRoutesMatchSpec(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey}})
	specSet, liveSet := map[routeKey]struct{}{}, map[routeKey]struct{}{}
	for path, item := range loadOpenAPI(t).Paths {
		if !strings.HasPrefix(path, "/inference/v1/") {
			continue
		}
		for method := range item {
			if _, ok := httpMethodKeys[method]; ok {
				specSet[rk(method, path)] = struct{}{}
			}
		}
	}
	for _, route := range s.engine.Routes() {
		if strings.HasPrefix(route.Path, "/inference/v1/") {
			liveSet[rk(route.Method, normalizeGinPath(route.Path))] = struct{}{}
		}
	}
	require.Len(t, liveSet, 4)
	assert.Equal(t, liveSet, specSet)

	paths, ok := mapAt(loadSpecTree(t), "paths")
	require.True(t, ok)
	for path, item := range paths {
		if !strings.HasPrefix(path, "/inference/v1/") {
			continue
		}
		operations, ok := item.(map[string]any)
		require.True(t, ok)
		for method, value := range operations {
			if _, ok := httpMethodKeys[method]; !ok {
				continue
			}
			op, ok := value.(map[string]any)
			require.True(t, ok)
			assert.Equal(t, []any{
				map[string]any{"InferenceBearer": []any{}},
				map[string]any{"InferenceKey": []any{}},
			}, op["security"], "%s %s", method, path)
		}
	}
}
