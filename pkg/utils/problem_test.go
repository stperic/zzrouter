package utils

import (
	"strings"
	"testing"
)

func TestNewProblemDetails(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		title    string
		detail   string
		instance string
	}{
		{
			name:     "simple problem",
			status:   400,
			title:    "Bad Request",
			detail:   "Invalid parameter",
			instance: "/api/models",
		},
		{
			name:     "not found",
			status:   404,
			title:    "Not Found",
			detail:   "Model not found",
			instance: "/api/models/nonexistent",
		},
		{
			name:     "server error",
			status:   500,
			title:    "Internal Server Error",
			detail:   "Database connection failed",
			instance: "/api/start",
		},
		{
			name:     "with spaces in title",
			status:   403,
			title:    "Access Denied Due To Policy",
			detail:   "Insufficient permissions",
			instance: "/api/admin",
		},
		{
			name:     "empty detail",
			status:   400,
			title:    "Bad Request",
			detail:   "",
			instance: "/api/test",
		},
		{
			name:     "empty instance",
			status:   500,
			title:    "Node Error",
			detail:   "Something went wrong",
			instance: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NewProblemDetails(tt.status, tt.title, tt.detail, tt.instance)

			if result == nil {
				t.Fatal("NewProblemDetails() returned nil")
			}

			if result.Status != tt.status {
				t.Errorf("Status = %v, want %v", result.Status, tt.status)
			}

			if result.Title != tt.title {
				t.Errorf("Title = %v, want %v", result.Title, tt.title)
			}

			if result.Detail != tt.detail {
				t.Errorf("Detail = %v, want %v", result.Detail, tt.detail)
			}

			if result.Instance != tt.instance {
				t.Errorf("Instance = %v, want %v", result.Instance, tt.instance)
			}

			// Verify Type is generated correctly
			expectedSlug := strings.ToLower(strings.ReplaceAll(tt.title, " ", "-"))
			expectedType := "https://api.zzrouter.com/problems/" + expectedSlug
			if result.Type != expectedType {
				t.Errorf("Type = %v, want %v", result.Type, expectedType)
			}
		})
	}
}

func TestProblemDetails_TypeGeneration(t *testing.T) {
	tests := []struct {
		title        string
		expectedSlug string
	}{
		{
			title:        "Bad Request",
			expectedSlug: "bad-request",
		},
		{
			title:        "Not Found",
			expectedSlug: "not-found",
		},
		{
			title:        "Internal Server Error",
			expectedSlug: "internal-server-error",
		},
		{
			title:        "UPPERCASE TITLE",
			expectedSlug: "uppercase-title",
		},
		{
			title:        "multiple   spaces",
			expectedSlug: "multiple---spaces",
		},
		{
			title:        "no-spaces-here",
			expectedSlug: "no-spaces-here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			result := NewProblemDetails(400, tt.title, "", "")
			expectedType := "https://api.zzrouter.com/problems/" + tt.expectedSlug

			if result.Type != expectedType {
				t.Errorf("Type = %v, want %v", result.Type, expectedType)
			}
		})
	}
}

func TestNewOpenAIError(t *testing.T) {
	param := "model"
	code := "invalid_value"

	result := NewOpenAIError("invalid_request_error", "Test message", &param, &code)

	if result == nil {
		t.Fatal("NewOpenAIError() returned nil")
	}

	if result.Error.Type != "invalid_request_error" {
		t.Errorf("Type = %v, want %v", result.Error.Type, "invalid_request_error")
	}

	if result.Error.Message != "Test message" {
		t.Errorf("Message = %v, want %v", result.Error.Message, "Test message")
	}

	if result.Error.Param == nil || *result.Error.Param != "model" {
		t.Errorf("Param = %v, want %v", result.Error.Param, "model")
	}

	if result.Error.Code == nil || *result.Error.Code != "invalid_value" {
		t.Errorf("Code = %v, want %v", result.Error.Code, "invalid_value")
	}
}

func TestNewOpenAIError_NilParams(t *testing.T) {
	result := NewOpenAIError("server_error", "Test message", nil, nil)

	if result == nil {
		t.Fatal("NewOpenAIError() returned nil")
	}

	if result.Error.Param != nil {
		t.Errorf("Param should be nil, got %v", result.Error.Param)
	}

	if result.Error.Code != nil {
		t.Errorf("Code should be nil, got %v", result.Error.Code)
	}
}

func TestNewOpenAIInvalidRequest(t *testing.T) {
	tests := []struct {
		name    string
		message string
		param   *string
	}{
		{
			name:    "with param",
			message: "Invalid value",
			param:   new("temperature"),
		},
		{
			name:    "without param",
			message: "Missing required field",
			param:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NewOpenAIInvalidRequest(tt.message, tt.param)

			if result == nil {
				t.Fatal("NewOpenAIInvalidRequest() returned nil")
			}

			if result.Error.Type != "invalid_request_error" {
				t.Errorf("Type = %v, want %v", result.Error.Type, "invalid_request_error")
			}

			if result.Error.Message != tt.message {
				t.Errorf("Message = %v, want %v", result.Error.Message, tt.message)
			}

			if result.Error.Code != nil {
				t.Errorf("Code should be nil for generic invalid-request errors (reserved for specific sub-codes), got %v", *result.Error.Code)
			}

			if tt.param != nil {
				if result.Error.Param == nil {
					t.Error("Param should not be nil")
				} else if *result.Error.Param != *tt.param {
					t.Errorf("Param = %v, want %v", *result.Error.Param, *tt.param)
				}
			}
		})
	}
}

func TestNewOpenAINodeError(t *testing.T) {
	message := "Internal server error"
	result := NewOpenAINodeError(message)

	if result == nil {
		t.Fatal("NewOpenAINodeError() returned nil")
	}

	if result.Error.Type != "server_error" {
		t.Errorf("Type = %v, want %v", result.Error.Type, "server_error")
	}

	if result.Error.Message != message {
		t.Errorf("Message = %v, want %v", result.Error.Message, message)
	}

	if result.Error.Code != nil {
		t.Errorf("Code should be nil for generic server errors (reserved for specific sub-codes), got %v", *result.Error.Code)
	}

	if result.Error.Param != nil {
		t.Errorf("Param should be nil, got %v", result.Error.Param)
	}
}

func TestProblemDetails_AllFields(t *testing.T) {
	problem := NewProblemDetails(404, "Resource Not Found", "The requested resource was not found", "/api/resource/123")

	// Verify all fields are properly set
	if problem.Type == "" {
		t.Error("Type should not be empty")
	}
	if problem.Title == "" {
		t.Error("Title should not be empty")
	}
	if problem.Status == 0 {
		t.Error("Status should not be zero")
	}
	if problem.Detail == "" {
		t.Error("Detail should not be empty (in this test)")
	}
	if problem.Instance == "" {
		t.Error("Instance should not be empty (in this test)")
	}
}

func TestOpenAIError_Structure(t *testing.T) {
	// Test that the structure can be properly created and accessed
	errorDetail := OpenAIErrorDetail{
		Message: "test",
		Type:    "invalid_request_error",
		Param:   new("param1"),
		Code:    new("code1"),
	}

	openAIError := &OpenAIError{
		Error: errorDetail,
	}

	if openAIError.Error.Message != "test" {
		t.Error("Message not accessible through struct")
	}
	if openAIError.Error.Type != "invalid_request_error" {
		t.Error("Type not accessible through struct")
	}
	if openAIError.Error.Param == nil || *openAIError.Error.Param != "param1" {
		t.Error("Param not accessible through struct")
	}
	if openAIError.Error.Code == nil || *openAIError.Error.Code != "code1" {
		t.Error("Code not accessible through struct")
	}
}

// Benchmark tests
func BenchmarkNewProblemDetails(b *testing.B) {
	for i := 0; i < b.N; i++ {
		NewProblemDetails(404, "Not Found", "Resource not found", "/api/test")
	}
}

func BenchmarkNewOpenAIError(b *testing.B) {
	param := "model"
	code := "invalid_value"
	for i := 0; i < b.N; i++ {
		NewOpenAIError("invalid_request_error", "Test message", &param, &code)
	}
}

func TestSanitizeErrorMessage(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains []string // strings that should be in output
		excludes []string // strings that should NOT be in output
	}{
		{
			name:     "empty string",
			input:    "",
			contains: []string{},
			excludes: []string{},
		},
		{
			name:     "simple error no paths",
			input:    "connection refused",
			contains: []string{"connection refused"},
			excludes: []string{},
		},
		{
			name:     "unix absolute path",
			input:    "failed to open /home/user/config/node.yaml: permission denied",
			contains: []string{"failed to open", "permission denied", "[path]"},
			excludes: []string{"/home/user/config/node.yaml"},
		},
		{
			name:     "windows path",
			input:    `failed to read C:\Users\Admin\Documents\config.yaml`,
			contains: []string{"failed to read", "[path]"},
			excludes: []string{`C:\Users\Admin\Documents\config.yaml`},
		},
		{
			name:     "home directory path",
			input:    "cannot access ~/Library/Application Support/zzrouter/config",
			contains: []string{"cannot access", "[path]"},
			excludes: []string{"~/Library/Application Support"},
		},
		{
			name:     "go module path",
			input:    "error in github.com/stperic/zzrouter/internal/server/handler.go:42",
			contains: []string{"error in", "[module]"},
			excludes: []string{"github.com/stperic/zzrouter/internal/server"},
		},
		{
			name:     "internal package path",
			input:    "internal/server/auth_handler.go failed validation",
			contains: []string{"[module]", "failed validation"},
			excludes: []string{"internal/server/auth_handler.go"},
		},
		{
			// Bare package identifiers must still be redacted: only the
			// leading "/" of a URL path distinguishes a route from an
			// import path.
			name:     "bare internal package path is redacted",
			input:    "open failed in internal/server/auth",
			contains: []string{"open failed in", "[module]"},
			excludes: []string{"internal/server/auth"},
		},
		{
			name:     "bare pkg path is redacted",
			input:    "panic in pkg/keys/store: boom",
			contains: []string{"[module]", "boom"},
			excludes: []string{"pkg/keys/store"},
		},
		{
			name:     "source reference with line number is redacted",
			input:    "internal/server/access_control.go:412: denied",
			contains: []string{"[module]", "denied"},
			excludes: []string{"access_control.go", "412"},
		},
		{
			// The cluster dispatches over /zzrouter/v1/internal/*. Scrubbing
			// that as a module path told the operator a peer was unreachable
			// without saying which endpoint had been called.
			name:     "cluster route survives sanitization",
			input:    `unicast to worker-1 failed: Post "https://198.51.100.235:9091/zzrouter/v1/internal/runs/preview": context deadline exceeded`,
			contains: []string{"/zzrouter/v1/internal/runs/preview", "context deadline exceeded"},
			excludes: []string{"[module]"},
		},
		{
			name:     "compat route survives sanitization",
			input:    "no provider serves /v1/chat/completions on this node",
			contains: []string{"/v1/chat/completions"},
			excludes: []string{"[module]", "[path]"},
		},
		{
			name:     "multiple paths",
			input:    "copy /var/log/app.log to /tmp/backup/app.log failed",
			contains: []string{"copy", "to", "failed", "[path]"},
			excludes: []string{"/var/log/app.log", "/tmp/backup/app.log"},
		},
		{
			name:     "safe validation error",
			input:    "field 'model' is required",
			contains: []string{"field 'model' is required"},
			excludes: []string{},
		},
		{
			name:     "network error no paths",
			input:    "dial tcp 127.0.0.1:8080: connection refused",
			contains: []string{"dial tcp 127.0.0.1:8080: connection refused"},
			excludes: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeErrorMessage(tt.input)

			for _, s := range tt.contains {
				if !strings.Contains(result, s) {
					t.Errorf("result %q should contain %q", result, s)
				}
			}

			for _, s := range tt.excludes {
				if strings.Contains(result, s) {
					t.Errorf("result %q should NOT contain %q", result, s)
				}
			}
		})
	}
}

func BenchmarkSanitizeErrorMessage(b *testing.B) {
	errMsg := "failed to open /home/user/config/node.yaml: permission denied"
	for i := 0; i < b.N; i++ {
		SanitizeErrorMessage(errMsg)
	}
}
