// Package search provides unified search capabilities across multiple model apps.
//
// # Basic Usage
//
// For simple use cases, the package provides convenient functions that automatically
// discover API tokens and handle authentication:
//
//	import "search"
//
//	// Search across all apps
//	results, err := search.SearchAllApps("llama", 10)
//
//	// Search only Hugging Face models
//	hfResults, err := search.SearchHuggingFace("bert", []string{"text-classification"}, 5)
//
// # Advanced Usage with Client
//
// For more control, use the Client pattern which allows explicit token management,
// custom HTTP clients, and context support:
//
//	client := search.NewDefaultClient() // Auto-discovers token
//	// or
//	client := search.NewClientWithToken("your-token-here")
//	// or
//	client := search.NewClient(search.ClientOptions{
//		APIToken: "your-token",
//		Timeout:  10 * time.Second,
//	})
//
//	ctx := context.Background()
//	results, err := client.SearchModels(ctx, search.SearchParams{
//		Search: "gpt",
//		Pagination: search.PaginationParams{Limit: 20},
//	})
//
// # Authentication
//
// The package automatically discovers Hugging Face API tokens from:
// 1. HUGGING_FACE_TOKEN environment variable
// 2. ~/.cache/huggingface/token file (standard HF CLI location)
//
// If no token is found, requests work for public models but may have lower rate limits.
//
// # Backward Compatibility
//
// All existing function signatures are preserved for backward compatibility,
// but it's recommended to migrate to the Client pattern for new code.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// linkHeaderRegex is pre-compiled for parsing Link headers
var linkHeaderRegex = regexp.MustCompile(`<([^>]+)>;\s*rel="([^"]+)"`)

const defaultTimeout = 30 * time.Second

// Client manages communication with the Hugging Face Hub API.
type Client struct {
	httpClient *http.Client
	apiToken   string
	baseURL    string
}

// ClientOptions configures the Client
type ClientOptions struct {
	HTTPClient *http.Client
	APIToken   string
	BaseURL    string
	Timeout    time.Duration
}

// NewClient creates a new Hugging Face API client with custom options.
func NewClient(options ClientOptions) *Client {
	httpClient := options.HTTPClient
	if httpClient == nil {
		timeout := options.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		httpClient = &http.Client{Timeout: timeout}
	}

	baseURL := options.BaseURL
	if baseURL == "" {
		baseURL = "https://huggingface.co/api/models"
	}

	return &Client{
		httpClient: httpClient,
		apiToken:   options.APIToken,
		baseURL:    baseURL,
	}
}

// NewDefaultClient creates a new Hugging Face API client.
// It automatically discovers the API token from environment variables or HF CLI cache.
func NewDefaultClient() *Client {
	return NewClient(ClientOptions{APIToken: FindToken()})
}

// ModelInfo represents information about a model from the Hub API
type ModelInfo struct {
	ID            string         `json:"id"`
	Downloads     int            `json:"downloads"`
	Likes         int            `json:"likes"`
	TrendingScore float64        `json:"trendingScore,omitempty"`
	Tags          []string       `json:"tags"`
	PipelineTag   string         `json:"pipeline_tag"`
	LibraryName   string         `json:"library_name"`
	CreatedAt     string         `json:"createdAt"`
	LastModified  string         `json:"lastModified,omitempty"`
	Private       bool           `json:"private"`
	Safetensors   any            `json:"safetensors,omitempty"`
	CardData      map[string]any `json:"cardData,omitempty"` // Parsed YAML metadata from model card
	Siblings      []hfSibling    `json:"siblings,omitempty"` // Files in the repository
}

// hfSibling represents a file in the model repository
type hfSibling struct {
	Rfilename string  `json:"rfilename"`
	Size      float64 `json:"size"`
}

// parseLinkHeader parses Hugging Face Link headers into CursorInfo
func parseLinkHeader(linkHeader string) *CursorInfo {
	if linkHeader == "" {
		return nil
	}

	cursor := &CursorInfo{}

	// Use the pre-compiled regex for better performance
	matches := linkHeaderRegex.FindAllStringSubmatch(linkHeader, -1)

	for _, match := range matches {
		if len(match) < 3 {
			continue
		}

		urlStr := match[1]
		rel := match[2]

		// Parse the URL to extract cursor parameter
		if u, err := url.Parse(urlStr); err == nil {
			if cursorParam := u.Query().Get("cursor"); cursorParam != "" {
				switch rel {
				case "next":
					cursor.Next = cursorParam
				case "prev":
					cursor.Previous = cursorParam
				case "first":
					cursor.First = cursorParam
				case "last":
					cursor.Last = cursorParam
				}
			}
		}
	}

	return cursor
}

// SearchModels searches for models on the Hugging Face Hub with flexible parameters and pagination
func (c *Client) SearchModels(ctx context.Context, params SearchParams) (*SearchResponse[ModelInfo], error) {
	// Create the request manually to add authentication headers
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Build query parameters
	values := url.Values{}

	if params.Search != "" {
		values.Set("search", params.Search)
	}
	if params.Author != "" {
		values.Set("author", params.Author)
	}
	// Add filter tags if provided (HuggingFace uses comma-separated tags)
	if len(params.Filter) > 0 {
		values.Set("filter", strings.Join(params.Filter, ","))
	}
	if params.Sort != "" {
		values.Set("sort", params.Sort)
	}
	if params.Direction != 0 {
		values.Set("direction", fmt.Sprintf("%d", params.Direction))
	}
	if params.Pagination.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", params.Pagination.Limit))
	}
	if params.Full {
		// Use expand for targeted fields instead of full=true (more efficient)
		for _, field := range []string{"safetensors", "gguf", "siblings", "downloads", "likes", "tags", "createdAt", "lastModified", "trendingScore"} {
			values.Add("expand", field)
		}
	}

	// Handle cursor for pagination (if provided)
	if params.Pagination.Cursor != "" {
		values.Set("cursor", params.Pagination.Cursor)
	}

	// Set query parameters on the request
	req.URL.RawQuery = values.Encode()

	// Log the full URL for debugging
	fmt.Printf("🔍 HuggingFace API Request: %s\n", req.URL.String())

	// Add authentication header if token is provided
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}

	// Use the client's HTTP client to send the request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to search models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hugging Face API returned status %d for search request (URL: %s)", resp.StatusCode, req.URL.String())
	}

	var models []ModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, fmt.Errorf("failed to parse search response: %w", err)
	}

	// Parse Link header for pagination
	linkHeader := resp.Header.Get("Link")
	cursor := parseLinkHeader(linkHeader)

	// Calculate pagination info
	hasMore := cursor != nil && cursor.Next != ""

	response := &SearchResponse[ModelInfo]{
		Results:      models,
		Limit:        params.Pagination.Limit,
		HasMore:      hasMore,
		Cursor:       cursor,
		IsTotalExact: false, // Hugging Face API doesn't provide exact totals
		// Total would need to be provided by API or estimated
	}

	return response, nil
}

// EnrichSiblingsWithSizes fetches file sizes from HuggingFace tree API and adds them to siblings.
// Callers must pass a cancellable context — the previous bare http.Get used the default client
// with no timeout, so a slow HF response would stall the request handler indefinitely.
func EnrichSiblingsWithSizes(ctx context.Context, modelID string, siblings []hfSibling) ([]hfSibling, error) {
	// Fetch file tree from HuggingFace
	url := fmt.Sprintf("https://huggingface.co/api/models/%s/tree/main", modelID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return siblings, err
	}
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return siblings, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return siblings, fmt.Errorf("tree API returned status %d", resp.StatusCode)
	}

	var treeFiles []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&treeFiles); err != nil {
		return siblings, err
	}

	// Create a map of filename -> size
	sizeMap := make(map[string]int64)
	for _, file := range treeFiles {
		if file.Type == "file" {
			sizeMap[file.Path] = file.Size
		}
	}

	// Enrich siblings with sizes
	enriched := make([]hfSibling, len(siblings))
	for i, sib := range siblings {
		enriched[i] = sib
		// Add size if found
		if size, exists := sizeMap[sib.Rfilename]; exists {
			enriched[i].Size = float64(size)
		}
	}

	return enriched, nil
}
