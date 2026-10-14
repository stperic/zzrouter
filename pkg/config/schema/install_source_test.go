package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stretchr/testify/require"
)

func TestInstallSourceDefinitionIsStrictAndSelfContained(t *testing.T) {
	definition, err := schema.InstallDefinition()
	require.NoError(t, err)
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	require.NoError(t, compiler.AddResource("install.json", strings.NewReader(string(definition))))
	compiled, err := compiler.Compile("install.json")
	require.NoError(t, err)
	fixture := `{"runtimes":{"engine":{"source":{"version":"1.0.0","url":"https://example.org/release.tar.gz","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","strip_components":1,"requirements":"requirements.txt","entrypoint":"main.py"}}}}`
	var valid any
	require.NoError(t, json.Unmarshal([]byte(fixture), &valid))
	require.NoError(t, compiled.Validate(valid))
	for name, mutate := range map[string]func(map[string]any){
		"unknown key":       func(s map[string]any) { s["unknown"] = "anything" },
		"cut lock key":      func(s map[string]any) { s["dependency_lock"] = "lock.txt" },
		"wrong type":        func(s map[string]any) { s["entrypoint"] = []string{"main.py"} },
		"noninteger strip":  func(s map[string]any) { s["strip_components"] = 1.5 },
		"unsupported strip": func(s map[string]any) { s["strip_components"] = 0 },
		"invalid hash":      func(s map[string]any) { s["sha256"] = strings.Repeat("g", 64) },
		"unsafe version":    func(s map[string]any) { s["version"] = "../1" },
	} {
		t.Run(name, func(t *testing.T) {
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(fixture), &body))
			source := body["runtimes"].(map[string]any)["engine"].(map[string]any)["source"].(map[string]any)
			mutate(source)
			require.Error(t, compiled.Validate(body))
		})
	}
	for _, field := range []string{"version", "url", "sha256", "strip_components", "requirements", "entrypoint"} {
		t.Run("missing "+field, func(t *testing.T) {
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(fixture), &body))
			source := body["runtimes"].(map[string]any)["engine"].(map[string]any)["source"].(map[string]any)
			delete(source, field)
			require.Error(t, compiled.Validate(body))
		})
	}
}
