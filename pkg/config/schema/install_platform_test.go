package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stretchr/testify/require"
)

func TestPlatformInstallDefinitionIsStrictAndSelfContained(t *testing.T) {
	definition, err := schema.InstallDefinition()
	require.NoError(t, err)
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	require.NoError(t, compiler.AddResource("install.json", strings.NewReader(string(definition))))
	compiled, err := compiler.Compile("install.json")
	require.NoError(t, err)
	for _, tc := range []struct {
		branch string
		valid  bool
	}{
		{`{"darwin":{"companions":{"torch":{"build":"none"}},"verify":{"checks":["pip_check","imports","mps_available"]}}}`, true},
		{`{"windows":{"verify":{"imports":[]}}}`, true},
		{`{"macos":{"verify":{"checks":[]}}}`, false},
		{`{"darwin":{"source":{}}}`, false},
		{`{"darwin":{"platforms":{}}}`, false},
		{`{"darwin":{"companions":{"torch":{"build":"mps"}}}}`, false},
		{`{"darwin":{"verify":{"command":"run anything"}}}`, false},
	} {
		var body any
		require.NoError(t, json.Unmarshal([]byte(`{"runtimes":{"engine":{"platforms":`+tc.branch+`}}}`), &body))
		err := compiled.Validate(body)
		if tc.valid {
			require.NoError(t, err, tc.branch)
		} else {
			require.Error(t, err, tc.branch)
		}
	}
}

func TestGPUPlatformSchemas(t *testing.T) {
	for _, kind := range []struct {
		name, body string
		validate   func([]byte) error
	}{
		{"on-demand", "protocol: openai\nruntime: {port_range: [8188, 8188], base_port: 8188, execution: {type: python, command: python}}\n", schema.ValidateOnDemand},
		{"external", "protocol: ollama\nruntime: {endpoint: 'http://localhost:11434'}\n", schema.ValidateExternal},
	} {
		t.Run(kind.name, func(t *testing.T) {
			for _, tc := range []struct {
				branch string
				valid  bool
			}{
				{"{darwin: apple}", true},
				{"{darwin: apple, linux: nvidia, windows: nvidia}", true},
				{"{macos: apple}", false},
				{"{darwin: unknown}", false},
				{"{darwin: ''}", false},
				{"{darwin: {vendor: apple}}", false},
			} {
				err := kind.validate([]byte(kind.body + "requirements: {gpu: {vendor: nvidia, policy: required, platforms: " + tc.branch + "}}\n"))
				if tc.valid {
					require.NoError(t, err, tc.branch)
				} else {
					require.Error(t, err, tc.branch)
				}
			}
		})
	}
}
