package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpandEnv(t *testing.T) {
	os.Setenv("TEST_EXPAND_SET", "hello")
	defer os.Unsetenv("TEST_EXPAND_SET")
	os.Unsetenv("TEST_EXPAND_UNSET")

	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"plain var", "${TEST_EXPAND_SET}", "hello"},
		{"var with default, var set", "${TEST_EXPAND_SET:-fallback}", "hello"},
		{"var with default, var unset", "${TEST_EXPAND_UNSET:-fallback}", "fallback"},
		{"var without default, var unset", "${TEST_EXPAND_UNSET}", ""},
		{"no vars", "https://api.example.com", "https://api.example.com"},
		{"mixed", "https://${TEST_EXPAND_SET}.example.com/${TEST_EXPAND_UNSET:-v1}", "https://hello.example.com/v1"},
		{"default with special chars", "${TEST_EXPAND_UNSET:-https://your-resource.openai.azure.com/openai}", "https://your-resource.openai.azure.com/openai"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, ExpandEnv(tt.input))
		})
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	os.Setenv("TEST_NORMALIZE_ENDPOINT",
		"https://ai-medoyajune.openai.azure.com/openai/v1/")
	defer os.Unsetenv("TEST_NORMALIZE_ENDPOINT")

	tests := []struct {
		name   string
		input  string
		expect string
	}{
		// Azure documents endpoints as ".../openai/v1/" but zzrouter's
		// request paths already start with "/v1/". Without normalization
		// the proxied URL becomes ".../openai/v1/v1/chat/completions",
		// which Azure rejects with 404 (surfaced as 502 upstream). This
		// is the user-observed Azure chat 502 bug the normalization
		// fixes.
		{
			name:   "azure-style /openai/v1/ strips to /openai",
			input:  "https://ai.openai.azure.com/openai/v1/",
			expect: "https://ai.openai.azure.com/openai",
		},
		{
			name:   "azure-style /openai/v1 without trailing slash",
			input:  "https://ai.openai.azure.com/openai/v1",
			expect: "https://ai.openai.azure.com/openai",
		},
		{
			name:   "trailing slash only, no /v1",
			input:  "https://api.openai.com/",
			expect: "https://api.openai.com",
		},
		{
			name:   "bare base URL unchanged",
			input:  "https://api.openai.com",
			expect: "https://api.openai.com",
		},
		{
			name:   "multiple trailing slashes collapse",
			input:  "https://api.example.com/v1///",
			expect: "https://api.example.com",
		},
		{
			name:   "/v1 mid-path is preserved",
			input:  "https://gateway.example.com/proxy/v1/backend",
			expect: "https://gateway.example.com/proxy/v1/backend",
		},
		{
			name:   "env var expansion + azure normalization",
			input:  "${TEST_NORMALIZE_ENDPOINT}",
			expect: "https://ai-medoyajune.openai.azure.com/openai",
		},
		{
			name:   "unset env var with azure default",
			input:  "${TEST_UNSET:-https://ai.openai.azure.com/openai/v1/}",
			expect: "https://ai.openai.azure.com/openai",
		},
		{
			name:   "empty string",
			input:  "",
			expect: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, NormalizeEndpoint(tt.input))
		})
	}
}

func TestParseVarWithDefault(t *testing.T) {
	name, def, has := parseVarWithDefault("VAR")
	assert.Equal(t, "VAR", name)
	assert.Equal(t, "", def)
	assert.False(t, has)

	name, def, has = parseVarWithDefault("VAR:-default")
	assert.Equal(t, "VAR", name)
	assert.Equal(t, "default", def)
	assert.True(t, has)

	name, def, has = parseVarWithDefault("VAR:-")
	assert.Equal(t, "VAR", name)
	assert.Equal(t, "", def)
	assert.True(t, has)
}
