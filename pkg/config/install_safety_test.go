package config

import (
	"strings"
	"testing"
)

func TestValidateInstallVariants_RejectsAttackerHost(t *testing.T) {
	bad := []AppInstallVariant{
		{ID: "linux-amd64", URLPattern: "https://attacker.example.com/{version}/{artifact}"},
	}
	err := ValidateInstallVariants("ollama", bad)
	if err == nil {
		t.Fatal("expected attacker host rejection")
	}
	if !strings.Contains(err.Error(), "install URL rejected") {
		t.Errorf("error should explain rejection, got: %v", err)
	}
}

func TestValidateInstallVariants_AcceptsAllowedHost(t *testing.T) {
	good := []AppInstallVariant{
		{ID: "linux-amd64", URLPattern: "https://github.com/ollama/ollama/releases/download/v{version}/{artifact}"},
	}
	if err := ValidateInstallVariants("ollama", good); err != nil {
		t.Errorf("github.com pattern should pass for ollama: %v", err)
	}
}

func TestValidateInstallVariants_PerArtifactURLChecked(t *testing.T) {
	bad := []AppInstallVariant{
		{
			ID:         "windows-cuda",
			URLPattern: "https://github.com/ggml-org/llama.cpp/releases/download/{version}/{artifact}",
			Artifacts: []VariantArtifact{
				{Filename: "cudart.zip", URLPattern: "https://attacker.example.com/cudart/{version}.zip"},
			},
		},
	}
	err := ValidateInstallVariants("llamacpp", bad)
	if err == nil || !strings.Contains(err.Error(), "artifacts[0]") {
		t.Fatalf("per-artifact override must be checked, got: %v", err)
	}
}

func TestValidateInstallVariants_UnpinnedProviderBypasses(t *testing.T) {
	// vllm/mlx aren't pinned (pip-driven). Validate must not reject
	// an empty or arbitrary URL — that path is gated by pip itself.
	v := []AppInstallVariant{
		{ID: "noop", URLPattern: "https://anywhere.example/x"},
	}
	if err := ValidateInstallVariants("vllm", v); err != nil {
		t.Errorf("unpinned provider must bypass: %v", err)
	}
}
