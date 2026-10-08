package utils

import (
	"testing"
)

func TestNormalizeAppType(t *testing.T) {
	tests := []struct {
		name         string
		providerType string
		want         string
	}{
		{
			name:         "ollama lowercase",
			providerType: "ollama",
			want:         "ollama",
		},
		{
			name:         "ollama uppercase",
			providerType: "OLLAMA",
			want:         "ollama",
		},
		{
			name:         "ollama mixed case",
			providerType: "Ollama",
			want:         "ollama",
		},
		{
			name:         "vllm lowercase",
			providerType: "vllm",
			want:         "vllm",
		},
		{
			name:         "vLLM mixed case",
			providerType: "vLLM",
			want:         "vllm",
		},
		{
			name:         "llamacpp",
			providerType: "llamacpp",
			want:         "llamacpp",
		},
		{
			name:         "LlamaCpp mixed case",
			providerType: "LlamaCpp",
			want:         "llamacpp",
		},
		{
			name:         "mlx lowercase",
			providerType: "mlx",
			want:         "mlx",
		},
		{
			name:         "MLX uppercase",
			providerType: "MLX",
			want:         "mlx",
		},
		{
			name:         "custom provider",
			providerType: "CustomProvider",
			want:         "customprovider",
		},
		{
			name:         "empty string",
			providerType: "",
			want:         "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeAppType(tt.providerType)
			if got != tt.want {
				t.Errorf("NormalizeAppType(%q) = %v, want %v", tt.providerType, got, tt.want)
			}
		})
	}
}

func TestCheckHTTPStatus(t *testing.T) {
	tests := []struct {
		name         string
		statusCode   int
		responseBody []byte
		operation    string
		wantErr      bool
		errContains  string
	}{
		{
			name:         "success 200",
			statusCode:   200,
			responseBody: []byte(`{"status":"ok"}`),
			operation:    "test operation",
			wantErr:      false,
		},
		{
			name:         "success 201",
			statusCode:   201,
			responseBody: []byte(`{}`),
			operation:    "create operation",
			wantErr:      false,
		},
		{
			name:         "success 299",
			statusCode:   299,
			responseBody: []byte(`{}`),
			operation:    "operation",
			wantErr:      false,
		},
		{
			name:         "error 400",
			statusCode:   400,
			responseBody: []byte(`{}`),
			operation:    "test operation",
			wantErr:      true,
			errContains:  "test operation failed with status 400",
		},
		{
			name:         "error 404",
			statusCode:   404,
			responseBody: []byte(`{"error":"not found"}`),
			operation:    "fetch operation",
			wantErr:      true,
			errContains:  "not found",
		},
		{
			name:         "error 500",
			statusCode:   500,
			responseBody: []byte(`{"error":"internal server error"}`),
			operation:    "test operation",
			wantErr:      true,
			errContains:  "internal server error",
		},
		{
			name:         "error with no message",
			statusCode:   503,
			responseBody: []byte(`{}`),
			operation:    "service call",
			wantErr:      true,
			errContains:  "service call failed with status 503",
		},
		{
			name:         "error with invalid JSON",
			statusCode:   500,
			responseBody: []byte(`not json`),
			operation:    "operation",
			wantErr:      true,
			errContains:  "operation failed with status 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckHTTPStatus(tt.statusCode, tt.responseBody, tt.operation)

			if tt.wantErr {
				if err == nil {
					t.Errorf("CheckHTTPStatus() expected error, got nil")
					return
				}
				if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
					t.Errorf("CheckHTTPStatus() error = %v, want error containing %q", err, tt.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("CheckHTTPStatus() unexpected error = %v", err)
				}
			}
		})
	}
}

// Benchmark tests
func BenchmarkNormalizeAppType(b *testing.B) {
	providers := []string{"ollama", "OLLAMA", "vLLM", "llamacpp", "MLX"}
	for i := 0; i < b.N; i++ {
		for _, p := range providers {
			NormalizeAppType(p)
		}
	}
}

// Helper function (copied from formatting_test.go)
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
