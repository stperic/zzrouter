package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	tests := []struct {
		name     string
		options  ClientOptions
		expected Client
	}{
		{
			name:    "default_options",
			options: ClientOptions{},
			expected: Client{
				apiToken: "",
				baseURL:  "https://huggingface.co/api/models",
			},
		},
		{
			name: "custom_options",
			options: ClientOptions{
				APIToken: "custom-token",
				BaseURL:  "https://custom.api.com/models",
				Timeout:  10 * time.Second,
			},
			expected: Client{
				apiToken: "custom-token",
				baseURL:  "https://custom.api.com/models",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.options)

			if client.apiToken != tt.expected.apiToken {
				t.Errorf("apiToken = %q, want %q", client.apiToken, tt.expected.apiToken)
			}
			if client.baseURL != tt.expected.baseURL {
				t.Errorf("baseURL = %q, want %q", client.baseURL, tt.expected.baseURL)
			}
			if client.httpClient == nil {
				t.Error("httpClient should not be nil")
			}
		})
	}
}

func TestNewDefaultClient(t *testing.T) {
	client := NewDefaultClient()

	if client == nil {
		t.Fatal("NewDefaultClient() returned nil")
	}
	if client.httpClient == nil {
		t.Error("httpClient should not be nil")
	}
	if client.baseURL != "https://huggingface.co/api/models" {
		t.Errorf("baseURL = %q, want %q", client.baseURL, "https://huggingface.co/api/models")
	}
}

func TestParseLinkHeader(t *testing.T) {
	tests := []struct {
		name       string
		linkHeader string
		expected   *CursorInfo
	}{
		{
			name:       "empty_header",
			linkHeader: "",
			expected:   nil,
		},
		{
			name:       "single_next_link",
			linkHeader: `<https://api.example.com/models?cursor=abc123>; rel="next"`,
			expected: &CursorInfo{
				Next: "abc123",
			},
		},
		{
			name:       "multiple_links",
			linkHeader: `<https://api.example.com/models?cursor=first123>; rel="first", <https://api.example.com/models?cursor=prev456>; rel="prev", <https://api.example.com/models?cursor=next789>; rel="next", <https://api.example.com/models?cursor=last000>; rel="last"`,
			expected: &CursorInfo{
				First:    "first123",
				Previous: "prev456",
				Next:     "next789",
				Last:     "last000",
			},
		},
		{
			name:       "malformed_link",
			linkHeader: `<https://api.example.com/models>; rel="next"`,
			expected:   &CursorInfo{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseLinkHeader(tt.linkHeader)

			if tt.expected == nil {
				if result != nil {
					t.Errorf("parseLinkHeader() = %v, want nil", result)
				}
				return
			}

			if result == nil {
				t.Errorf("parseLinkHeader() = nil, want %v", tt.expected)
				return
			}

			if result.Next != tt.expected.Next {
				t.Errorf("Next = %q, want %q", result.Next, tt.expected.Next)
			}
			if result.Previous != tt.expected.Previous {
				t.Errorf("Previous = %q, want %q", result.Previous, tt.expected.Previous)
			}
			if result.First != tt.expected.First {
				t.Errorf("First = %q, want %q", result.First, tt.expected.First)
			}
			if result.Last != tt.expected.Last {
				t.Errorf("Last = %q, want %q", result.Last, tt.expected.Last)
			}
		})
	}
}

func TestClientSearchModels(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check authentication
		auth := r.Header.Get("Authorization")
		if auth != "" && !strings.HasPrefix(auth, "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Check path
		if r.URL.Path != "/api/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Mock response data
		models := []ModelInfo{
			{
				ID:        "test/model1",
				Downloads: 100,
				Likes:     10,
				Tags:      []string{"tag1", "tag2"},
			},
			{
				ID:        "test/model2",
				Downloads: 200,
				Likes:     20,
				Tags:      []string{"tag3"},
			},
		}

		// Add Link header for pagination
		w.Header().Set("Link", `<https://api.example.com/models?cursor=next123>; rel="next"`)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(models)
	}))
	defer server.Close()

	// Create client with test server URL
	client := NewClient(ClientOptions{
		BaseURL: server.URL + "/api/models",
	})

	// Test search
	params := SearchParams{
		Search: "test",
		Pagination: PaginationParams{
			Limit: 10,
		},
	}

	response, err := client.SearchModels(context.Background(), params)
	if err != nil {
		t.Fatalf("SearchModels failed: %v", err)
	}

	// Verify response
	if len(response.Results) != 2 {
		t.Errorf("Expected 2 results, got %d", len(response.Results))
	}

	if response.Results[0].ID != "test/model1" {
		t.Errorf("First result ID = %q, want %q", response.Results[0].ID, "test/model1")
	}

	if response.Results[1].Downloads != 200 {
		t.Errorf("Second result downloads = %d, want %d", response.Results[1].Downloads, 200)
	}

	if !response.HasMore {
		t.Error("HasMore should be true")
	}

	if response.Cursor == nil || response.Cursor.Next != "next123" {
		t.Errorf("Cursor.Next = %q, want %q", response.Cursor.Next, "next123")
	}
}

func TestClientGetRepoFiles(t *testing.T) {
	// This test requires complex HTTP mocking and is skipped for now
	t.Skip("getRepoFiles test requires more complex HTTP mocking")
}
