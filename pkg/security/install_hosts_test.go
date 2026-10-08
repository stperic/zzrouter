package security

import "testing"

func TestValidateInstallHost(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		url      string
		wantErr  bool
	}{
		{"ollama github ok", "ollama",
			"https://github.com/ollama/ollama/releases/download/v0.3.0/ollama-windows-amd64.zip", false},
		{"ollama github subdomain ok (api.github.com)", "ollama",
			"https://api.github.com/repos/ollama/ollama/releases/latest", false},
		{"ollama attacker host rejected", "ollama",
			"https://attacker.example.com/ollama/releases/v0.3.0/x.zip", true},
		{"ollama pypi rejected", "ollama",
			"https://pypi.org/simple/ollama", true},
		{"ollama.com rejected (no checksum surface)", "ollama",
			"https://ollama.com/download/ollama-linux-amd64.tgz", true},
		{"llamacpp github ok", "llamacpp",
			"https://github.com/ggml-org/llama.cpp/releases/download/b6000/llama-bin.zip", false},
		{"llamacpp non-https rejected", "llamacpp",
			"http://github.com/ggml-org/llama.cpp/releases/download/b6000/llama-bin.zip", true},
		{"unregistered provider passes (no pin)", "vllm",
			"https://attacker.example.com/wheel.whl", false},
		{"placeholder url accepted (host extracted ok)", "ollama",
			"https://github.com/ollama/ollama/releases/download/v{version}/{artifact}", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateInstallHost(tt.provider, tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("err=%v want err=%v", err, tt.wantErr)
			}
		})
	}
}

func TestHasInstallHostAllowlist(t *testing.T) {
	if !HasInstallHostAllowlist("ollama") {
		t.Error("ollama should be pinned")
	}
	if HasInstallHostAllowlist("vllm") {
		t.Error("vllm not yet pinned (pip-only)")
	}
}
