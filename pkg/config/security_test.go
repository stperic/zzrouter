package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestIsEnvVarName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// Environment variable names (should return true)
		{"uppercase with underscores", "ZZROUTER_ADMIN_API_KEY", true},
		{"single word uppercase", "APIKEY", true},
		{"with numbers", "API_KEY_123", true},
		{"starts with underscore", "_API_KEY", true},
		{"multiple underscores", "MY_APP_API_KEY", true},

		// Literal values (should return false)
		{"lowercase", "my-dev-key", false},
		{"mixed case", "MyDevKey", false},
		{"with hyphens", "MY-API-KEY", false},
		{"with spaces", "MY API KEY", false},
		{"random string", "abc123xyz", false},
		{"base64-like", "dGVzdC1rZXktMTIz", false},
		{"uuid-like", "550e8400-e29b-41d4-a716-446655440000", false},
		{"hex string", "a1b2c3d4e5f6", false},
		{"only numbers", "123456", false},
		{"only underscores", "___", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isEnvVarName(tt.input)
			if got != tt.want {
				t.Errorf("isEnvVarName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveAPIKey_EnvVar(t *testing.T) {
	// Set up test environment variable
	testEnvVar := "TEST_API_KEY_12345"
	testValue := "secret-key-from-env"
	_ = os.Setenv(testEnvVar, testValue)
	defer func() { _ = os.Unsetenv(testEnvVar) }()

	// Test resolving from environment variable
	got, err := resolveAPIKey(testEnvVar, "")
	if err != nil {
		t.Fatalf("resolveAPIKey() error = %v", err)
	}
	if got != testValue {
		t.Errorf("resolveAPIKey() = %q, want %q", got, testValue)
	}
}

func TestResolveAPIKey_LiteralValue(t *testing.T) {
	// Test using literal value (development mode)
	literalKey := "my-dev-key-123"
	got, err := resolveAPIKey(literalKey, "")
	if err != nil {
		t.Fatalf("resolveAPIKey() error = %v", err)
	}
	if got != literalKey {
		t.Errorf("resolveAPIKey() = %q, want %q", got, literalKey)
	}
}

func TestResolveAPIKey_EnvVarNotSet(t *testing.T) {
	// Test error when env var is not set
	envVar := "NONEXISTENT_ENV_VAR_XYZ"
	_ = os.Unsetenv(envVar) // Ensure it's not set

	_, err := resolveAPIKey(envVar, "")
	if err == nil {
		t.Error("resolveAPIKey() expected error for unset env var, got nil")
	}
}

func TestResolveAPIKey_FallbackToDefault(t *testing.T) {
	// Set up default environment variable
	defaultEnvVar := "DEFAULT_API_KEY"
	defaultValue := "default-secret-key"
	_ = os.Setenv(defaultEnvVar, defaultValue)
	defer func() { _ = os.Unsetenv(defaultEnvVar) }()

	// Try to resolve non-existent env var with default fallback
	nonExistentVar := "NONEXISTENT_VAR"
	_ = os.Unsetenv(nonExistentVar)

	got, err := resolveAPIKey(nonExistentVar, defaultEnvVar)
	if err != nil {
		t.Fatalf("resolveAPIKey() error = %v", err)
	}
	if got != defaultValue {
		t.Errorf("resolveAPIKey() = %q, want %q (from default)", got, defaultValue)
	}
}

func TestNodeConnection_GetAPIKey_EnvVar(t *testing.T) {
	// Set up test environment
	envVar := "TEST_CLIENT_KEY"
	envValue := "client-secret-456"
	_ = os.Setenv(envVar, envValue)
	defer func() { _ = os.Unsetenv(envVar) }()

	conn := &NodeConnection{
		Key: envVar,
	}

	got, err := conn.GetAPIKey()
	if err != nil {
		t.Fatalf("GetAPIKey() error = %v", err)
	}
	if got != envValue {
		t.Errorf("GetAPIKey() = %q, want %q", got, envValue)
	}
}

func TestNodeConnection_GetAPIKey_LiteralValue(t *testing.T) {
	// Test literal value (development mode)
	literalKey := "my-dev-client-key"
	conn := &NodeConnection{
		Key: literalKey,
	}

	got, err := conn.GetAPIKey()
	if err != nil {
		t.Fatalf("GetAPIKey() error = %v", err)
	}
	if got != literalKey {
		t.Errorf("GetAPIKey() = %q, want %q", got, literalKey)
	}
}

func TestValidateAPIKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		// Valid keys (32+ chars with good entropy)
		{"valid strong key", "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6", false},
		{"valid random key", "xK9mP2nQ5rT8wY3zA6bC1dE4fG7hJ0kL", false},
		{"valid generated key", "mx-abcdefghijkXYZ7890ABCDEFGHIJK", false},

		// Invalid: too short (minimum 32 chars)
		{"empty key", "", true},
		{"too short", "short", true},
		{"30 chars (below minimum)", "xK9mP2nQ5rT8wY3zA6bC1dE4fG7hJ0", true},

		// Invalid: contains weak patterns (even if long enough)
		{"contains 'password'", "my-password-key-1234567890abcdef", true},
		{"contains 'admin'", "admin-key-123456789012345678901234", true},
		{"contains 'test'", "test-key-12345678901234567890123456", true},
		{"contains 'default'", "default-api-key-12345678901234567", true},
		{"contains 'secret'", "secret-key-1234567890abcdefghijk", true},

		// Invalid: low entropy (too many repeated chars)
		{"low entropy (repeated)", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},
		{"low entropy (single dominant)", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAPIKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateAPIKey() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSmartDetectionExamples(t *testing.T) {
	// This test documents the expected behavior with real-world examples
	examples := []struct {
		description string
		value       string
		isEnvVar    bool
	}{
		// Production: Environment variables
		{"production env var", "ZZROUTER_ADMIN_API_KEY", true},
		{"production env var alt", "PROD_API_KEY", true},
		{"kubernetes secret ref", "K8S_SECRET_API_KEY", true},

		// Development: Literal values
		{"dev literal key", "dev-key-12345", false},
		{"base64 token", "dGVzdC1rZXktMTIzNDU2Nzg5MA==", false},
		{"uuid", "550e8400-e29b-41d4-a716-446655440000", false},
		{"random string", "xK9mP2nQ5rT8wY3zA6bC1dE4fG7hJ0", false},
		{"simple dev key", "mydevkey123", false},
	}

	for _, ex := range examples {
		t.Run(ex.description, func(t *testing.T) {
			got := isEnvVarName(ex.value)
			if got != ex.isEnvVar {
				t.Errorf("%s: isEnvVarName(%q) = %v, want %v",
					ex.description, ex.value, got, ex.isEnvVar)
			}
		})
	}
}

func TestGenerateAPIKey(t *testing.T) {
	// Test generating multiple keys
	for i := range 10 {
		key, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey() failed: %v", err)
		}

		// Validate the generated key meets security requirements
		if err := ValidateAPIKey(key); err != nil {
			t.Errorf("Generated key failed validation: %v", err)
		}

		// Check key has mx- prefix
		if !strings.HasPrefix(key, "mx-") {
			t.Errorf("Generated key should start with 'mx-' prefix, got: %s", key)
		}

		// Check key length (should be 46 chars: 3 for "mx-" + 43 for base64)
		if len(key) != 46 {
			t.Errorf("Generated key length = %d, want 46", len(key))
		}

		// Check that keys are unique
		if i > 0 {
			// Generate a second key and ensure it's different
			key2, err := GenerateAPIKey()
			if err != nil {
				t.Fatalf("GenerateAPIKey() failed for second key: %v", err)
			}
			if key == key2 {
				t.Error("Generated keys are not unique")
			}
		}

		t.Logf("Generated key %d: %s", i+1, key)
	}
}

// TestAuthConfig tests the new flatter client authentication structure
func TestAuthConfig_GetAdminAPIKey(t *testing.T) {
	tests := []struct {
		name        string
		config      AuthConfig
		setupEnv    func()
		cleanupEnv  func()
		wantKey     string
		wantErr     bool
		wantErrIs   error  // optional: require errors.Is match
		errContains string // optional: substring check (env var name, typically)
	}{
		{
			name: "admin key from environment variable",
			config: AuthConfig{
				AdminKey: "ZZROUTER_ADMIN_API_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_ADMIN_API_KEY", "admin-secret-from-env")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_ADMIN_API_KEY")
			},
			wantKey: "admin-secret-from-env",
			wantErr: false,
		},
		{
			name: "admin key as literal value",
			config: AuthConfig{
				AdminKey: "my-dev-admin-key",
			},
			setupEnv:   func() {},
			cleanupEnv: func() {},
			wantKey:    "my-dev-admin-key",
			wantErr:    false,
		},
		{
			name: "admin key from custom env var",
			config: AuthConfig{
				AdminKey: "CUSTOM_ADMIN_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("CUSTOM_ADMIN_KEY", "custom-admin-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("CUSTOM_ADMIN_KEY")
			},
			wantKey: "custom-admin-secret",
			wantErr: false,
		},
		{
			name: "admin key env var not set",
			config: AuthConfig{
				AdminKey: "NONEXISTENT_ADMIN_KEY",
			},
			setupEnv:    func() {},
			cleanupEnv:  func() {},
			wantKey:     "",
			wantErr:     true,
			wantErrIs:   ErrMissingEnvKey,
			errContains: "NONEXISTENT_ADMIN_KEY",
		},
		{
			name: "admin key empty config",
			config: AuthConfig{
				AdminKey: "",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_ADMIN_API_KEY", "default-admin-key")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_ADMIN_API_KEY")
			},
			wantKey: "default-admin-key",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupEnv()
			defer tt.cleanupEnv()

			gotKey, err := tt.config.GetAdminAPIKey()

			if tt.wantErr {
				if err == nil {
					t.Errorf("GetAdminAPIKey() expected error, got nil")
					return
				}
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Errorf("GetAdminAPIKey() error = %v, expected errors.Is(%v)", err, tt.wantErrIs)
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("GetAdminAPIKey() error = %v, expected to contain %s", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("GetAdminAPIKey() unexpected error = %v", err)
				return
			}

			if gotKey != tt.wantKey {
				t.Errorf("GetAdminAPIKey() = %q, want %q", gotKey, tt.wantKey)
			}
		})
	}
}

func TestAuthConfig_GetUserAPIKey(t *testing.T) {
	tests := []struct {
		name        string
		config      AuthConfig
		setupEnv    func()
		cleanupEnv  func()
		wantKey     string
		wantErr     bool
		wantErrIs   error  // optional: require errors.Is match
		errContains string // optional: substring check (env var name, typically)
	}{
		{
			name: "user key from environment variable",
			config: AuthConfig{
				UserKey: "ZZROUTER_API_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_API_KEY", "user-secret-from-env")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_API_KEY")
			},
			wantKey: "user-secret-from-env",
			wantErr: false,
		},
		{
			name: "user key as literal value",
			config: AuthConfig{
				UserKey: "my-dev-user-key",
			},
			setupEnv:   func() {},
			cleanupEnv: func() {},
			wantKey:    "my-dev-user-key",
			wantErr:    false,
		},
		{
			name: "user key from custom env var",
			config: AuthConfig{
				UserKey: "CUSTOM_USER_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("CUSTOM_USER_KEY", "custom-user-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("CUSTOM_USER_KEY")
			},
			wantKey: "custom-user-secret",
			wantErr: false,
		},
		{
			name: "user key env var not set",
			config: AuthConfig{
				UserKey: "NONEXISTENT_USER_KEY",
			},
			setupEnv:    func() {},
			cleanupEnv:  func() {},
			wantKey:     "",
			wantErr:     true,
			wantErrIs:   ErrMissingEnvKey,
			errContains: "NONEXISTENT_USER_KEY",
		},
		{
			name: "user key empty config",
			config: AuthConfig{
				UserKey: "",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_API_KEY", "default-user-key")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_API_KEY")
			},
			wantKey: "default-user-key",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupEnv()
			defer tt.cleanupEnv()

			gotKey, err := tt.config.GetUserAPIKey()

			if tt.wantErr {
				if err == nil {
					t.Errorf("GetUserAPIKey() expected error, got nil")
					return
				}
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Errorf("GetUserAPIKey() error = %v, expected errors.Is(%v)", err, tt.wantErrIs)
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("GetUserAPIKey() error = %v, expected to contain %s", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("GetUserAPIKey() unexpected error = %v", err)
				return
			}

			if gotKey != tt.wantKey {
				t.Errorf("GetUserAPIKey() = %q, want %q", gotKey, tt.wantKey)
			}
		})
	}
}

// TestAuthConfig_Combinations tests all combinations of admin and user keys
func TestAuthConfig_Combinations(t *testing.T) {
	tests := []struct {
		name         string
		config       AuthConfig
		setupEnv     func()
		cleanupEnv   func()
		wantAdminKey string
		wantUserKey  string
		wantAdminErr bool
		wantUserErr  bool
	}{
		{
			name: "both keys from environment variables",
			config: AuthConfig{
				AdminKey: "ZZROUTER_ADMIN_API_KEY",
				UserKey:  "ZZROUTER_API_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_ADMIN_API_KEY", "admin-secret")
				_ = os.Setenv("ZZROUTER_API_KEY", "user-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_ADMIN_API_KEY")
				_ = os.Unsetenv("ZZROUTER_API_KEY")
			},
			wantAdminKey: "admin-secret",
			wantUserKey:  "user-secret",
			wantAdminErr: false,
			wantUserErr:  false,
		},
		{
			name: "admin from env, user as literal",
			config: AuthConfig{
				AdminKey: "ZZROUTER_ADMIN_API_KEY",
				UserKey:  "my-dev-user-key",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_ADMIN_API_KEY", "admin-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_ADMIN_API_KEY")
			},
			wantAdminKey: "admin-secret",
			wantUserKey:  "my-dev-user-key",
			wantAdminErr: false,
			wantUserErr:  false,
		},
		{
			name: "admin as literal, user from env",
			config: AuthConfig{
				AdminKey: "my-dev-admin-key",
				UserKey:  "ZZROUTER_API_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_API_KEY", "user-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_API_KEY")
			},
			wantAdminKey: "my-dev-admin-key",
			wantUserKey:  "user-secret",
			wantAdminErr: false,
			wantUserErr:  false,
		},
		{
			name: "both keys as literals",
			config: AuthConfig{
				AdminKey: "my-dev-admin-key",
				UserKey:  "my-dev-user-key",
			},
			setupEnv:     func() {},
			cleanupEnv:   func() {},
			wantAdminKey: "my-dev-admin-key",
			wantUserKey:  "my-dev-user-key",
			wantAdminErr: false,
			wantUserErr:  false,
		},
		{
			name: "admin from custom env, user from different custom env",
			config: AuthConfig{
				AdminKey: "CUSTOM_ADMIN_KEY",
				UserKey:  "CUSTOM_USER_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("CUSTOM_ADMIN_KEY", "custom-admin-secret")
				_ = os.Setenv("CUSTOM_USER_KEY", "custom-user-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("CUSTOM_ADMIN_KEY")
				_ = os.Unsetenv("CUSTOM_USER_KEY")
			},
			wantAdminKey: "custom-admin-secret",
			wantUserKey:  "custom-user-secret",
			wantAdminErr: false,
			wantUserErr:  false,
		},
		{
			name: "admin env not set, user env works",
			config: AuthConfig{
				AdminKey: "NONEXISTENT_ADMIN_KEY",
				UserKey:  "ZZROUTER_API_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_API_KEY", "user-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_API_KEY")
			},
			wantAdminKey: "",
			wantUserKey:  "user-secret",
			wantAdminErr: true,
			wantUserErr:  false,
		},
		{
			name: "admin env works, user env not set",
			config: AuthConfig{
				AdminKey: "ZZROUTER_ADMIN_API_KEY",
				UserKey:  "NONEXISTENT_USER_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_ADMIN_API_KEY", "admin-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_ADMIN_API_KEY")
			},
			wantAdminKey: "admin-secret",
			wantUserKey:  "",
			wantAdminErr: false,
			wantUserErr:  true,
		},
		{
			name: "both env vars not set",
			config: AuthConfig{
				AdminKey: "NONEXISTENT_ADMIN_KEY",
				UserKey:  "NONEXISTENT_USER_KEY",
			},
			setupEnv:     func() {},
			cleanupEnv:   func() {},
			wantAdminKey: "",
			wantUserKey:  "",
			wantAdminErr: true,
			wantUserErr:  true,
		},
		{
			name: "empty config uses defaults",
			config: AuthConfig{
				AdminKey: "",
				UserKey:  "",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_ADMIN_API_KEY", "default-admin")
				_ = os.Setenv("ZZROUTER_API_KEY", "default-user")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_ADMIN_API_KEY")
				_ = os.Unsetenv("ZZROUTER_API_KEY")
			},
			wantAdminKey: "default-admin",
			wantUserKey:  "default-user",
			wantAdminErr: false,
			wantUserErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupEnv()
			defer tt.cleanupEnv()

			// Test admin key
			adminKey, adminErr := tt.config.GetAdminAPIKey()
			if tt.wantAdminErr {
				if adminErr == nil {
					t.Errorf("GetAdminAPIKey() expected error, got nil")
				}
			} else {
				if adminErr != nil {
					t.Errorf("GetAdminAPIKey() unexpected error = %v", adminErr)
				}
				if adminKey != tt.wantAdminKey {
					t.Errorf("GetAdminAPIKey() = %q, want %q", adminKey, tt.wantAdminKey)
				}
			}

			// Test user key
			userKey, userErr := tt.config.GetUserAPIKey()
			if tt.wantUserErr {
				if userErr == nil {
					t.Errorf("GetUserAPIKey() expected error, got nil")
				}
			} else {
				if userErr != nil {
					t.Errorf("GetUserAPIKey() unexpected error = %v", userErr)
				}
				if userKey != tt.wantUserKey {
					t.Errorf("GetUserAPIKey() = %q, want %q", userKey, tt.wantUserKey)
				}
			}
		})
	}
}

// TestAuthConfig_RealWorldScenarios tests real-world configuration scenarios
func TestAuthConfig_RealWorldScenarios(t *testing.T) {
	tests := []struct {
		name        string
		description string
		config      AuthConfig
		setupEnv    func()
		cleanupEnv  func()
		expectAdmin string
		expectUser  string
		expectError bool
	}{
		{
			name:        "production environment",
			description: "Both keys from environment variables in production",
			config: AuthConfig{
				AdminKey: "ZZROUTER_ADMIN_API_KEY",
				UserKey:  "ZZROUTER_API_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_ADMIN_API_KEY", "prod-admin-secret-123")
				_ = os.Setenv("ZZROUTER_API_KEY", "prod-user-secret-456")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_ADMIN_API_KEY")
				_ = os.Unsetenv("ZZROUTER_API_KEY")
			},
			expectAdmin: "prod-admin-secret-123",
			expectUser:  "prod-user-secret-456",
			expectError: false,
		},
		{
			name:        "development environment",
			description: "Literal keys for development",
			config: AuthConfig{
				AdminKey: "dev-admin-key",
				UserKey:  "dev-user-key",
			},
			setupEnv:    func() {},
			cleanupEnv:  func() {},
			expectAdmin: "dev-admin-key",
			expectUser:  "dev-user-key",
			expectError: false,
		},
		{
			name:        "kubernetes deployment",
			description: "Keys from Kubernetes secrets",
			config: AuthConfig{
				AdminKey: "K8S_SECRET_ADMIN_KEY",
				UserKey:  "K8S_SECRET_USER_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("K8S_SECRET_ADMIN_KEY", "k8s-admin-from-secret")
				_ = os.Setenv("K8S_SECRET_USER_KEY", "k8s-user-from-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("K8S_SECRET_ADMIN_KEY")
				_ = os.Unsetenv("K8S_SECRET_USER_KEY")
			},
			expectAdmin: "k8s-admin-from-secret",
			expectUser:  "k8s-user-from-secret",
			expectError: false,
		},
		{
			name:        "docker deployment",
			description: "Keys from Docker secrets/environment",
			config: AuthConfig{
				AdminKey: "DOCKER_ADMIN_KEY",
				UserKey:  "DOCKER_USER_KEY",
			},
			setupEnv: func() {
				_ = os.Setenv("DOCKER_ADMIN_KEY", "docker-admin-secret")
				_ = os.Setenv("DOCKER_USER_KEY", "docker-user-secret")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("DOCKER_ADMIN_KEY")
				_ = os.Unsetenv("DOCKER_USER_KEY")
			},
			expectAdmin: "docker-admin-secret",
			expectUser:  "docker-user-secret",
			expectError: false,
		},
		{
			name:        "mixed deployment",
			description: "Admin from env, user as literal for testing",
			config: AuthConfig{
				AdminKey: "ZZROUTER_ADMIN_API_KEY",
				UserKey:  "test-user-key",
			},
			setupEnv: func() {
				_ = os.Setenv("ZZROUTER_ADMIN_API_KEY", "prod-admin-key")
			},
			cleanupEnv: func() {
				_ = os.Unsetenv("ZZROUTER_ADMIN_API_KEY")
			},
			expectAdmin: "prod-admin-key",
			expectUser:  "test-user-key",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Scenario: %s", tt.description)

			tt.setupEnv()
			defer tt.cleanupEnv()

			adminKey, adminErr := tt.config.GetAdminAPIKey()
			userKey, userErr := tt.config.GetUserAPIKey()

			hasError := adminErr != nil || userErr != nil

			if hasError != tt.expectError {
				t.Errorf("Expected error: %v, got adminErr: %v, userErr: %v",
					tt.expectError, adminErr, userErr)
				return
			}

			if !tt.expectError {
				if adminKey != tt.expectAdmin {
					t.Errorf("Admin key = %q, want %q", adminKey, tt.expectAdmin)
				}
				if userKey != tt.expectUser {
					t.Errorf("User key = %q, want %q", userKey, tt.expectUser)
				}
			}

			t.Logf("✓ Admin key: %s", adminKey)
			t.Logf("✓ User key: %s", userKey)
		})
	}
}
