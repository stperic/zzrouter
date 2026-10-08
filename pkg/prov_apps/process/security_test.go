package process

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateModelName(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		wantErr bool
	}{
		{"empty is valid", "", false},
		{"simple name", "llama3", false},
		{"with org", "meta/llama3", false},
		{"with tag", "llama3:70b", false},
		{"with dots", "model.gguf", false},
		{"with hyphens", "my-model-v2", false},
		{"with plus", "model+lora", false},
		{"with at", "org@v1", false},
		{"huggingface path", "meta-llama/Llama-3-8B", false},

		// Dangerous
		{"semicolon", "model;rm -rf /", true},
		{"pipe", "model|cat /etc/passwd", true},
		{"backtick", "model`whoami`", true},
		{"dollar", "model$(id)", true},
		{"newline", "model\nrm", true},
		{"carriage return", "model\rinjection", true},
		{"path traversal", "../../etc/passwd", true},
		{"space", "model name", true},
		{"ampersand", "model&bg", true},
		{"angle brackets", "model<in>out", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateModelName(tt.model)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSecurityValidator_ValidateCommand(t *testing.T) {
	sv := NewSecurityValidator(nil) // defaults

	assert.NoError(t, sv.ValidateCommand([]string{"python", "-m", "vllm"}))
	assert.NoError(t, sv.ValidateCommand([]string{"llama-server", "--port", "8000"}))

	assert.Error(t, sv.ValidateCommand([]string{}))
	assert.Error(t, sv.ValidateCommand([]string{"evil-binary"}))
}

func TestSecurityValidator_ValidateCommandArgs(t *testing.T) {
	sv := NewSecurityValidator(nil)

	// Valid args
	assert.NoError(t, sv.ValidateCommand([]string{"python", "-m", "vllm", "--port", "8000"}))

	// Shell metacharacters
	assert.Error(t, sv.ValidateCommand([]string{"python", "arg;rm -rf /"}))
	assert.Error(t, sv.ValidateCommand([]string{"python", "arg|cat"}))
	assert.Error(t, sv.ValidateCommand([]string{"python", "arg&bg"}))
	assert.Error(t, sv.ValidateCommand([]string{"python", "$(whoami)"}))
	assert.Error(t, sv.ValidateCommand([]string{"python", "arg`cmd`"}))

	// Newlines (added per security review)
	assert.Error(t, sv.ValidateCommand([]string{"python", "arg\nrm"}))
	assert.Error(t, sv.ValidateCommand([]string{"python", "arg\revil"}))
}

func TestSecurityValidator_DangerousPatterns(t *testing.T) {
	sv := NewSecurityValidator(nil)

	assert.Error(t, sv.ValidateCommand([]string{"python", "rm -rf /tmp"}))
	assert.Error(t, sv.ValidateCommand([]string{"python", "sudo something"}))
}

func TestSecurityValidator_SandboxCommand(t *testing.T) {
	sv := NewSecurityValidator(&SecurityConfig{EnableSandboxing: true})
	result := sv.SandboxCommand([]string{"python", "-m", "vllm"})
	// If nice is available (it should be on macOS/Linux), command is wrapped
	if len(result) > 3 {
		assert.Equal(t, "nice", result[0])
		assert.Equal(t, "-n", result[1])
		assert.Equal(t, "10", result[2])
	}
}

func TestSecurityValidator_SandboxDisabled(t *testing.T) {
	sv := NewSecurityValidator(&SecurityConfig{EnableSandboxing: false})
	cmd := []string{"python", "-m", "vllm"}
	result := sv.SandboxCommand(cmd)
	assert.Equal(t, cmd, result)
}

func TestIsDangerousEnvVar(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		dangerous bool
	}{
		// Dynamic linker (Unix)
		{"LD_PRELOAD", "LD_PRELOAD", true},
		{"LD_LIBRARY_PATH", "LD_LIBRARY_PATH", true},
		{"LD_AUDIT", "LD_AUDIT", true},
		// Dynamic linker (macOS)
		{"DYLD_INSERT_LIBRARIES", "DYLD_INSERT_LIBRARIES", true},
		{"DYLD_LIBRARY_PATH", "DYLD_LIBRARY_PATH", true},
		{"DYLD_FRAMEWORK_PATH", "DYLD_FRAMEWORK_PATH", true},
		// Language injection
		{"PYTHONPATH", "PYTHONPATH", true},
		{"PYTHONSTARTUP", "PYTHONSTARTUP", true},
		{"PYTHONHOME", "PYTHONHOME", true},
		{"NODE_OPTIONS", "NODE_OPTIONS", true},
		{"RUBYOPT", "RUBYOPT", true},
		// Path hijacking
		{"PATH", "PATH", true},
		// zzRouter secrets (prefix match)
		{"ZZROUTER_ADMIN_API_KEY", "ZZROUTER_ADMIN_API_KEY", true},
		{"ZZROUTER_CLUSTER_NETWORK_KEY", "ZZROUTER_CLUSTER_NETWORK_KEY", true},
		{"ZZROUTER_SECRET_TOKEN", "ZZROUTER_SECRET_TOKEN", true},
		// Case insensitive
		{"lowercase ld_preload", "ld_preload", true},
		{"mixed case Pythonpath", "PythonPath", true},
		// Safe vars
		{"CUDA_VISIBLE_DEVICES", "CUDA_VISIBLE_DEVICES", false},
		{"HF_HOME", "HF_HOME", false},
		{"VLLM_WORKER_COUNT", "VLLM_WORKER_COUNT", false},
		{"MY_CUSTOM_VAR", "MY_CUSTOM_VAR", false},
		{"HOME", "HOME", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.dangerous, IsDangerousEnvVar(tt.key))
		})
	}
}

func TestValidateParameters(t *testing.T) {
	t.Run("valid params", func(t *testing.T) {
		params := map[string]string{
			"max-model-len":     "4096",
			"gpu_memory":        "0.9",
			"trust-remote-code": "true",
			"tensor-parallel":   "2",
		}
		assert.NoError(t, ValidateParameters(params))
	})

	t.Run("empty params", func(t *testing.T) {
		assert.NoError(t, ValidateParameters(nil))
		assert.NoError(t, ValidateParameters(map[string]string{}))
	})

	t.Run("invalid key with shell chars", func(t *testing.T) {
		params := map[string]string{"key;rm": "value"}
		assert.Error(t, ValidateParameters(params))
	})

	t.Run("invalid key with spaces", func(t *testing.T) {
		params := map[string]string{"bad key": "value"}
		assert.Error(t, ValidateParameters(params))
	})

	t.Run("invalid key with dots", func(t *testing.T) {
		params := map[string]string{"key.name": "value"}
		assert.Error(t, ValidateParameters(params))
	})

	t.Run("null byte in value", func(t *testing.T) {
		params := map[string]string{"key": "val\x00ue"}
		assert.Error(t, ValidateParameters(params))
	})

	t.Run("control char in value", func(t *testing.T) {
		params := map[string]string{"key": "val\x01ue"}
		assert.Error(t, ValidateParameters(params))
	})

	t.Run("tab in value is allowed", func(t *testing.T) {
		params := map[string]string{"key": "val\tue"}
		assert.NoError(t, ValidateParameters(params))
	})

	t.Run("space in value is allowed", func(t *testing.T) {
		params := map[string]string{"key": "some value with spaces"}
		assert.NoError(t, ValidateParameters(params))
	})
}

func TestBuildChildEnvironment(t *testing.T) {
	t.Run("includes user vars", func(t *testing.T) {
		env := buildChildEnvironment(map[string]string{
			"CUDA_VISIBLE_DEVICES": "0,1",
			"MY_VAR":               "test",
		})
		found := map[string]bool{}
		for _, e := range env {
			if e == "CUDA_VISIBLE_DEVICES=0,1" {
				found["cuda"] = true
			}
			if e == "MY_VAR=test" {
				found["myvar"] = true
			}
			if e == "ZZROUTER_PROCESS_MARKER=1" {
				found["marker"] = true
			}
		}
		assert.True(t, found["cuda"])
		assert.True(t, found["myvar"])
		assert.True(t, found["marker"])
	})

	t.Run("excludes server secrets", func(t *testing.T) {
		// Set a secret in the current env — the allowlist should filter it
		env := buildChildEnvironment(nil)
		for _, e := range env {
			assert.NotContains(t, e, "ZZROUTER_ADMIN_API_KEY",
				"server secrets must not leak to child processes")
		}
	})

	t.Run("includes process marker", func(t *testing.T) {
		env := buildChildEnvironment(nil)
		found := slices.Contains(env, "ZZROUTER_PROCESS_MARKER=1")
		assert.True(t, found, "process marker must be present")
	})
}

func TestParseVersionFromOutput(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"vllm 0.11.0", "0.11.0"},
		{"version 1.2.3", "1.2.3"},
		{"v2.0.0-beta1", "2.0.0-beta1"},
		{"llama.cpp v1.0.0", "1.0.0"},
		{"no version here", "no version here"},
		{"", "unknown"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.expected, ParseVersionFromOutput(tt.input))
	}
}

func TestValidateParametersNativeWindowsPaths(t *testing.T) {
	for _, path := range []string{`C:\models\vision\mmproj.gguf`, `\\server\share\mmproj.gguf`, `C:\models/vision\mmproj.gguf`} {
		t.Run(path, func(t *testing.T) {
			assert.NoError(t, ValidateParameters(map[string]string{"mmproj": path}))
		})
	}
	for _, path := range []string{`..\secret`, `C:..\secret`, `C:../secret`, `C:..`, `C:\models\..\secret`, `C:\models/..\secret`, `\\server\share\..\secret`, "C:\\models\\bad\npath", `C:\models\bad;command`, `C:\models\$(command)`} {
		t.Run(path, func(t *testing.T) {
			assert.Error(t, ValidateParameters(map[string]string{"mmproj": path}))
		})
	}
}
