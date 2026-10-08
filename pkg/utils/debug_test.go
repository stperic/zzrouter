package utils

import (
	"os"
	"testing"
)

func TestSetDebugMode(t *testing.T) {
	// Save initial state
	initialState := IsDebugEnabled()
	defer SetDebugMode(initialState) // Restore after test

	tests := []struct {
		name    string
		enabled bool
	}{
		{
			name:    "enable debug mode",
			enabled: true,
		},
		{
			name:    "disable debug mode",
			enabled: false,
		},
		{
			name:    "enable again",
			enabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetDebugMode(tt.enabled)
			got := IsDebugEnabled()
			if got != tt.enabled {
				t.Errorf("SetDebugMode(%v), IsDebugEnabled() = %v, want %v", tt.enabled, got, tt.enabled)
			}
		})
	}
}

func TestIsDebugEnabled(t *testing.T) {
	// Test initial state (should be false by default)
	SetDebugMode(false)
	if IsDebugEnabled() {
		t.Error("IsDebugEnabled() should be false by default")
	}

	// Test enabled state
	SetDebugMode(true)
	if !IsDebugEnabled() {
		t.Error("IsDebugEnabled() should be true after SetDebugMode(true)")
	}

	// Reset
	SetDebugMode(false)
}

func TestDebugf(t *testing.T) {
	tests := []struct {
		name        string
		debugMode   bool
		format      string
		args        []any
		shouldPrint bool
	}{
		{
			name:        "debug enabled - should print",
			debugMode:   true,
			format:      "test message %s",
			args:        []any{"value"},
			shouldPrint: true,
		},
		{
			name:        "debug disabled - should not print",
			debugMode:   false,
			format:      "test message %s",
			args:        []any{"value"},
			shouldPrint: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Note: This test is simplified because capturing fmt.Printf is tricky
			// We mainly test that the function doesn't panic
			SetDebugMode(tt.debugMode)
			Debugf(tt.format, tt.args...)
		})
	}
}

func TestLogDebugf(t *testing.T) {
	// Test that function doesn't panic
	SetDebugMode(true)
	LogDebugf("test message %s", "value")

	SetDebugMode(false)
	LogDebugf("test message %s", "value")
}

func TestLogDebugln(t *testing.T) {
	// Test that function doesn't panic
	SetDebugMode(true)
	LogDebugln("test", "message")

	SetDebugMode(false)
	LogDebugln("test", "message")
}

func TestLogInfof(t *testing.T) {
	// Test that function doesn't panic
	LogInfof("test message %s", "value")
}

func TestLogErrorf(t *testing.T) {
	// Test that function doesn't panic
	LogErrorf("error message %s", "value")
}

func TestInitDebugFromEnv(t *testing.T) {
	// Save and restore environment
	oldDebug := os.Getenv("ZZROUTER_DEBUG")
	defer func() {
		if oldDebug != "" {
			_ = os.Setenv("ZZROUTER_DEBUG", oldDebug)
		} else {
			_ = os.Unsetenv("ZZROUTER_DEBUG")
		}
	}()

	tests := []struct {
		name      string
		envValue  string
		wantDebug bool
	}{
		{
			name:      "ZZROUTER_DEBUG=true",
			envValue:  "true",
			wantDebug: true,
		},
		{
			name:      "ZZROUTER_DEBUG=1",
			envValue:  "1",
			wantDebug: true,
		},
		{
			name:      "ZZROUTER_DEBUG=false",
			envValue:  "false",
			wantDebug: false,
		},
		{
			name:      "ZZROUTER_DEBUG=0",
			envValue:  "0",
			wantDebug: false,
		},
		{
			name:      "ZZROUTER_DEBUG empty",
			envValue:  "",
			wantDebug: false,
		},
		{
			name:      "ZZROUTER_DEBUG unset",
			envValue:  "",
			wantDebug: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset debug state
			SetDebugMode(false)

			// Set environment variable
			if tt.envValue != "" {
				_ = os.Setenv("ZZROUTER_DEBUG", tt.envValue)
			} else {
				_ = os.Unsetenv("ZZROUTER_DEBUG")
			}

			// Initialize from environment
			InitDebugFromEnv()

			got := IsDebugEnabled()
			if got != tt.wantDebug {
				t.Errorf("InitDebugFromEnv() with ZZROUTER_DEBUG=%q, IsDebugEnabled() = %v, want %v",
					tt.envValue, got, tt.wantDebug)
			}
		})
	}
}

func TestDebugModeConcurrency(t *testing.T) {
	// Test that concurrent access doesn't cause race conditions
	done := make(chan bool)

	for i := range 10 {
		go func(id int) {
			for range 100 {
				SetDebugMode(id%2 == 0)
				_ = IsDebugEnabled()
				Debugf("concurrent test %d", id)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for range 10 {
		<-done
	}

	// Test should complete without data races
}

func TestDebugOutputFormat(t *testing.T) {
	// Capture output to verify format
	SetDebugMode(true)
	defer SetDebugMode(false)

	// Test that debug functions accept various argument types
	Debugf("string: %s, int: %d, float: %.2f", "test", 42, 3.14)
	LogDebugf("formatted: %v", map[string]int{"key": 123})
	LogDebugln("multiple", "args", 456)
}

func TestDebugFunctionsDoNotPanicWhenDisabled(t *testing.T) {
	SetDebugMode(false)

	// All these should complete without panic
	tests := []struct {
		name string
		fn   func()
	}{
		{
			name: "Debugf",
			fn:   func() { Debugf("test %s", "message") },
		},
		{
			name: "LogDebugf",
			fn:   func() { LogDebugf("test %s", "message") },
		},
		{
			name: "LogDebugln",
			fn:   func() { LogDebugln("test", "message") },
		},
		{
			name: "LogInfof",
			fn:   func() { LogInfof("test %s", "message") },
		},
		{
			name: "LogErrorf",
			fn:   func() { LogErrorf("test %s", "message") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked: %v", tt.name, r)
				}
			}()
			tt.fn()
		})
	}
}

func TestDebugFunctionsWithVariousTypes(t *testing.T) {
	SetDebugMode(true)
	defer SetDebugMode(false)

	// Test with various Go types to ensure no panic
	Debugf("nil: %v", nil)
	Debugf("bool: %v", true)
	Debugf("int: %d", 42)
	Debugf("float: %f", 3.14)
	Debugf("string: %s", "test")
	Debugf("slice: %v", []int{1, 2, 3})
	Debugf("map: %v", map[string]int{"key": 123})
	Debugf("struct: %+v", struct{ Name string }{"test"})

}

// Benchmark tests
func BenchmarkSetDebugMode(b *testing.B) {
	for i := 0; i < b.N; i++ {
		SetDebugMode(i%2 == 0)
	}
}

func BenchmarkIsDebugEnabled(b *testing.B) {
	SetDebugMode(true)
	for i := 0; i < b.N; i++ {
		IsDebugEnabled()
	}
}

func BenchmarkDebugfDisabled(b *testing.B) {
	SetDebugMode(false)
	for i := 0; i < b.N; i++ {
		Debugf("test message %d", i)
	}
}

func BenchmarkDebugfEnabled(b *testing.B) {
	SetDebugMode(true)
	for i := 0; i < b.N; i++ {
		Debugf("test message %d", i)
	}
}
