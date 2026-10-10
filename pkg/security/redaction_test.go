package security

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactSensitive(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string // Should NOT contain this after redaction
	}{
		{
			name:     "zzRouter API key",
			input:    "Using key mx-abcdefghij1234567890ABCDEFGHIJK",
			contains: "abcdefghij1234567890",
		},
		{
			name:     "OpenAI-style key",
			input:    "Authorization: Bearer sk-1234567890abcdefghijklmnop",
			contains: "1234567890abcdefghijklmnop",
		},
		{
			name:     "X-API-Key header",
			input:    "X-API-Key: mx-secretkey12345678901234567890",
			contains: "secretkey12345678901234567890",
		},
		{
			name:     "password in string",
			input:    "password=mysecretpassword123",
			contains: "mysecretpassword123",
		},
		{
			name:     "ZZROUTER env var",
			input:    "ZZROUTER_ADMIN_API_KEY=mx-verysecretkey1234567890abc",
			contains: "verysecretkey1234567890abc",
		},
		{
			name:     "no sensitive data",
			input:    "Just a normal log message",
			contains: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := RedactSensitive(tt.input)

			if tt.contains != "" && containsString(result, tt.contains) {
				t.Errorf("RedactSensitive() should have redacted '%s', got: %s", tt.contains, result)
			}

			if tt.contains != "" && !containsString(result, RedactedPlaceholder) {
				t.Errorf("RedactSensitive() should contain %s, got: %s", RedactedPlaceholder, result)
			}
		})
	}
}

func TestExpandEnvWhitelisted(t *testing.T) {
	// Set up test environment variables
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("ZZROUTER_ADMIN_API_KEY", "secret-should-not-expand")
	t.Setenv("CUDA_VISIBLE_DEVICES", "0,1")

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "whitelisted HOME",
			input:    "${HOME}/models",
			expected: "/home/testuser/models",
		},
		{
			name:     "whitelisted CUDA",
			input:    "CUDA=${CUDA_VISIBLE_DEVICES}",
			expected: "CUDA=0,1",
		},
		{
			name:     "blocked sensitive key",
			input:    "KEY=${ZZROUTER_ADMIN_API_KEY}",
			expected: "KEY=${ZZROUTER_ADMIN_API_KEY}", // Should NOT expand
		},
		{
			name:     "mixed",
			input:    "${HOME}/config with key ${ZZROUTER_ADMIN_API_KEY}",
			expected: "/home/testuser/config with key ${ZZROUTER_ADMIN_API_KEY}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExpandEnvWhitelisted(tt.input)
			if result != tt.expected {
				t.Errorf("ExpandEnvWhitelisted(%s) = %s, want %s", tt.input, result, tt.expected)
			}
		})
	}
}

func containsString(s, substr string) bool {
	return len(substr) > 0 && len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestRedactSensitive_QuotedFieldsInDiagnostics(t *testing.T) {
	for _, input := range []string{
		`RuntimeError: response={"token":"invented-private-credential"}`,
		`RuntimeError: response={"token":"invented-private-credential's-suffix"}`,
		`RuntimeError: response={'token': 'invented-private-credential"suffix'}`,
		`RuntimeError: response={"token":"invented-private-credential\"suffix"}`,
		`ValueError: response={'password': 'invented-private-credential'}`,
		`RuntimeError: response={"access_token":"invented-private-credential","client_secret":"invented-private-credential"}`,
		`RuntimeError: response={'oauth_token': 'invented-private-credential', 'proxy-password': 'invented-private-credential'}`,
		`RuntimeError: response={"session_credential":"invented-private-credential\"suffix"}`,
		`RuntimeError: response={"X-API-Key":"invented-private-credential"}`,
		`ERROR: upstream {"authorization": "invented-private-credential", "message":"connection failed"}`,
	} {
		t.Run(input, func(t *testing.T) {
			result := RedactSensitive(input)
			assert.NotContains(t, result, "invented-private-credential")
			assert.NotContains(t, result, "suffix")
			assert.Contains(t, result, RedactedPlaceholder)
		})
	}
}

func TestRedactSensitive_QuotedPricingIsNotCredential(t *testing.T) {
	input := `RuntimeError: pricing={"output_cost_per_token":"0.00001"}`
	assert.Equal(t, input, RedactSensitive(input))
}
