package constants

import "strings"

// Repository types (model sources/registries)
const (
	RepoOllama      = "ollama"
	RepoHuggingFace = "huggingface"
	RepoHFAlias     = "hf" // Short alias for HuggingFace
	RepoGGUF        = "gguf"
	RepoLocal       = "local"
)

// Repository aliases for user input
var RepoAliases = map[string]string{
	"ollama":      RepoOllama,
	"huggingface": RepoHuggingFace,
	"hf":          RepoHuggingFace,
	"gguf":        RepoGGUF,
	"local":       RepoLocal,
}

// Application types (inference servers/serving applications)
const (
	AppOllama   = "ollama"
	AppVLLM     = "vllm"
	AppLlamaCpp = "llama.cpp"
	AppTGI      = "tgi"
)

// Application aliases for user input
var AppAliases = map[string]string{
	"ollama":                    AppOllama,
	"vllm":                      AppVLLM,
	"llama.cpp":                 AppLlamaCpp,
	"llamacpp":                  AppLlamaCpp,
	"llama-cpp":                 AppLlamaCpp,
	"tgi":                       AppTGI,
	"text-generation-inference": AppTGI,
	"textgen":                   AppTGI,
}

// NormalizeRepo normalizes repository name from user input
func NormalizeRegistry(repo string) string {
	if repo == "" {
		return "*" // All repos
	}

	// Check aliases (case-insensitive)
	lower := strings.ToLower(repo)
	if normalized, ok := RepoAliases[lower]; ok {
		return normalized
	}

	return repo
}

// NormalizeApp normalizes application name from user input
func NormalizeApp(app string) string {
	if app == "" {
		return ""
	}

	// Check aliases (case-insensitive)
	lower := strings.ToLower(app)
	if normalized, ok := AppAliases[lower]; ok {
		return normalized
	}

	return app
}

// IsKnownRepo checks if a string is a known repository type
func IsKnownRegistry(name string) bool {
	lower := strings.ToLower(name)
	_, exists := RepoAliases[lower]
	return exists
}

// IsKnownApp checks if a string is a known application type
func IsKnownApp(name string) bool {
	lower := strings.ToLower(name)
	_, exists := AppAliases[lower]
	return exists
}
