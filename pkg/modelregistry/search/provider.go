package search

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
)

// Provider identifies the model source a search runs against.
type Provider string

const (
	ProviderHuggingFace Provider = "huggingface"
	ProviderOllama      Provider = "ollama"
	ProviderAll         Provider = "all"
)

func (p Provider) String() string { return string(p) }

// IsValid returns true for a known built-in or a cloud provider registered
// via RegisterCloudProvider. Unregistered providers are rejected.
func (p Provider) IsValid() bool {
	switch p {
	case ProviderHuggingFace, ProviderOllama, ProviderAll:
		return true
	default:
		return IsCloudSearchProvider(p)
	}
}

// ParseProvider converts a user-supplied string to a Provider, accepting
// built-in and cloud-provider names.
func ParseProvider(s string) (Provider, error) {
	lower := strings.ToLower(s)
	switch lower {
	case constants.RepoHuggingFace, constants.RepoHFAlias:
		return ProviderHuggingFace, nil
	case constants.RepoOllama:
		return ProviderOllama, nil
	case "all", "":
		return ProviderAll, nil
	default:
		p := Provider(lower)
		if IsCloudSearchProvider(p) {
			return p, nil
		}
		return "", fmt.Errorf("unknown provider: %s (valid options: huggingface, ollama, %s, all)", s, strings.Join(validProviderNames(), ", "))
	}
}

// validProviderNames returns registered cloud provider names for error messages.
func validProviderNames() []string {
	providers := CloudProviders()
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = string(p)
	}
	return names
}
