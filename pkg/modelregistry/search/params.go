package search

import "fmt"

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
