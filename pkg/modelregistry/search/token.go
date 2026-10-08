package search

import (
	"os"
	"path/filepath"
	"strings"
)

// hfTokenEnvVars is the lookup order for HuggingFace API tokens.
// HF_TOKEN is the current standard; HUGGING_FACE_TOKEN is an older variable
// still emitted by some tooling, so we keep it as a fallback.
var hfTokenEnvVars = []string{"HF_TOKEN", "HUGGING_FACE_TOKEN"}

// FindToken discovers a HuggingFace API token from the environment or the
// default HF CLI cache location (~/.cache/huggingface/token).
//
// Returns an empty string when no token is found. An empty token is valid —
// requests to public endpoints still succeed, they just carry lower rate
// limits.
//
// Canonical lookup for the entire module: both pkg/modelregistry's
// download connector and the search client share this helper. Do not
// reintroduce package-local copies — a split would mean
// "HF auth worked over here, failed over there" bugs that are painful to
// track down.
func FindToken() string {
	for _, env := range hfTokenEnvVars {
		if token := os.Getenv(env); token != "" {
			return token
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	tokenPath := filepath.Join(home, ".cache", "huggingface", "token")
	if data, err := os.ReadFile(tokenPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}
