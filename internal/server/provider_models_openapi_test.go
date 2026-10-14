package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderModelsOpenAPIReferencesAndExamples(t *testing.T) {
	document, err := embeddedOpenAPI()
	require.NoError(t, err)
	paths := document["paths"].(map[string]any)
	item := paths["/zzrouter/v1/providers/{name}/models"].(map[string]any)
	seen := map[string]bool{}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok && strings.HasPrefix(ref, "#/") && !seen[ref] {
				seen[ref] = true
				var target any = document
				for _, part := range strings.Split(ref[2:], "/") {
					mapping, ok := target.(map[string]any)
					require.True(t, ok, "invalid reference %s", ref)
					target, ok = mapping[part]
					require.True(t, ok, "unresolved reference %s", ref)
				}
				walk(target)
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(item)
	resolve := paths["/zzrouter/v1/providers/{name}/models/resolve"].(map[string]any)["post"].(map[string]any)
	walk(resolve)
	require.Equal(t, "resolveProviderModels", resolve["operationId"])
	for _, status := range []string{"200", "202"} {
		response := resolve["responses"].(map[string]any)[status].(map[string]any)
		content := response["content"].(map[string]any)["application/json"].(map[string]any)
		require.Contains(t, content, "example")
	}
	examples := resolve["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["examples"].(map[string]any)
	for _, example := range examples {
		body, err := json.Marshal(example.(map[string]any)["value"])
		require.NoError(t, err)
		var req providerModelResolveRequest
		require.NoError(t, json.Unmarshal(body, &req))
		items, err := workflowRequirements(req.Workflow)
		require.NoError(t, err)
		require.Len(t, items, 1)
	}
	require.True(t, seen["#/components/responses/ProviderModelProblem"])
	for method, status := range map[string]string{"get": "200", "post": "202", "delete": "200"} {
		op := item[method].(map[string]any)
		responses := op["responses"].(map[string]any)
		response := responses[status].(map[string]any)
		content := response["content"].(map[string]any)["application/json"].(map[string]any)
		require.Contains(t, content, "example")
	}
	getSpec := paths["/openapi.yaml"].(map[string]any)["get"].(map[string]any)
	require.Contains(t, getSpec["responses"], "200", "new components must not change the served-spec operation")
}
