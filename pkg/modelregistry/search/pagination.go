package search

import "fmt"

// MaxResultsLimit is the upper bound on page size for any search request.
const MaxResultsLimit = 1000

// PaginationParams represents pagination parameters for API requests.
type PaginationParams struct {
	Offset int    `json:"offset,omitempty"` // Number of results to skip (for Ollama, memory-based pagination)
	Limit  int    `json:"limit,omitempty"`  // Maximum number of results to return
	Cursor string `json:"cursor,omitempty"` // Cursor for API-based pagination (Hugging Face)
}

// Validate checks if pagination parameters are valid.
func (p *PaginationParams) Validate() error {
	if p.Limit < 0 {
		return fmt.Errorf("%w: %d", ErrLimitNegative, p.Limit)
	}
	if p.Limit > MaxResultsLimit {
		return fmt.Errorf("%w (%d): got %d", ErrLimitExceedsMax, MaxResultsLimit, p.Limit)
	}
	if p.Offset < 0 {
		return fmt.Errorf("%w: %d", ErrOffsetNegative, p.Offset)
	}
	return nil
}

// CursorInfo represents cursor information for pagination.
type CursorInfo struct {
	Next     string `json:"next,omitempty"`  // Cursor for next page
	Previous string `json:"prev,omitempty"`  // Cursor for previous page
	First    string `json:"first,omitempty"` // Cursor for first page
	Last     string `json:"last,omitempty"`  // Cursor for last page
}

// SearchResponse represents a paginated search response.
type SearchResponse[T any] struct {
	Results      []T         `json:"results"`
	Total        int         `json:"total,omitempty"`    // Total number of results available (estimated for HF)
	IsTotalExact bool        `json:"is_total_exact"`     // Whether Total is an exact count or estimate
	Offset       int         `json:"offset,omitempty"`   // Current offset (for Ollama)
	Limit        int         `json:"limit,omitempty"`    // Current limit
	HasMore      bool        `json:"has_more,omitempty"` // Whether there are more results available
	Cursor       *CursorInfo `json:"cursor,omitempty"`   // Cursor information for API-based pagination
}
