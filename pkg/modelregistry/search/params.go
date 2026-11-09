package search

import (
	"fmt"
	"slices"
)

// SearchParams describes a model search request against any Provider.
type SearchParams struct {
	Provider   Provider         `json:"provider"`            // Provider to search (huggingface, ollama, or all)
	Search     string           `json:"search,omitempty"`    // Substring filter on repo + username
	Author     string           `json:"author,omitempty"`    // Filter by author or organization
	Filter     []string         `json:"filter,omitempty"`    // Tag filter (e.g., "text-classification", "pytorch")
	Sort       string           `json:"sort,omitempty"`      // Sort key ("downloads", "likes", "lastModified", "createdAt", "author")
	Direction  int              `json:"direction,omitempty"` // -1 for descending; any other value for ascending
	Pagination PaginationParams `json:"pagination"`
	Full       bool             `json:"full,omitempty"` // Include full metadata in each result
}

func (sp *SearchParams) Validate() error {
	if !sp.Provider.IsValid() {
		return fmt.Errorf("%w: %s", ErrInvalidProvider, sp.Provider)
	}
	allowed := []string{"", "id", "name", "title", "date", "createdAt"}
	switch sp.Provider {
	case ProviderHuggingFace, ProviderAll:
		allowed = []string{"", "date", "popular", "downloads", "likes", "lastModified", "createdAt", "trendingScore", "author"}
	case ProviderOllama:
		allowed = []string{"", "id", "name", "title", "downloads", "pulls", "popular", "lastModified", "modified", "updated", "newest", "size"}
	}
	if !slices.Contains(allowed, sp.Sort) {
		return fmt.Errorf("unsupported sort %q for %s; allowed: %v", sp.Sort, sp.Provider, allowed)
	}
	if sp.Provider != ProviderAll && sp.Provider != ProviderHuggingFace && (sp.Author != "" || len(sp.Filter) > 0) {
		return fmt.Errorf("author and tags filters require huggingface")
	}
	if sp.Pagination.Cursor != "" && (sp.Provider != ProviderAll && sp.Provider != ProviderHuggingFace || sp.Pagination.Offset != 0) {
		return fmt.Errorf("cursor requires huggingface with offset=0")
	}
	return sp.Pagination.Validate()
}

// RepoFileInfo describes a file in a HuggingFace repository as returned
// by the model-card API (the `siblings[].rfilename` shape). See the
// package metadata documentation for why this
// stays distinct from pkg/modelregistry/metadata.TreeFileEntry.
type RepoFileInfo struct {
	Name string `json:"rfilename"`
	Size int64  `json:"size"`
}
