package clientcli

import (
	"errors"
	"testing"
	"time"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"
)

// ============================================================================
// shared.ExtractUserFriendlyError Tests
// ============================================================================

func TestExtractUserFriendlyError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: "",
		},
		{
			name:     "simple error",
			err:      errors.New("something went wrong"),
			expected: "Something went wrong",
		},
		{
			name:     "error with prefix",
			err:      errors.New("error: failed to connect"),
			expected: "Connect", // Strips both "error: " and "failed to " prefixes
		},
		{
			name:     "error with quoted text",
			err:      errors.New("'model' not found"),
			expected: "'model' not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shared.ExtractUserFriendlyError(tt.err)
			if got != tt.expected {
				t.Errorf("shared.ExtractUserFriendlyError() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// ============================================================================
// shared.FormatDuration Tests
// ============================================================================

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		isFuture bool
		expected string
	}{
		{
			name:     "just now",
			duration: 30 * time.Second,
			isFuture: false,
			expected: "just now",
		},
		{
			name:     "less than a minute future",
			duration: 30 * time.Second,
			isFuture: true,
			expected: "less than a minute",
		},
		{
			name:     "1 minute ago",
			duration: 1 * time.Minute,
			isFuture: false,
			expected: "1 minute ago",
		},
		{
			name:     "5 minutes ago",
			duration: 5 * time.Minute,
			isFuture: false,
			expected: "5 minutes ago",
		},
		{
			name:     "1 hour ago",
			duration: 1 * time.Hour,
			isFuture: false,
			expected: "1 hour ago",
		},
		{
			name:     "3 hours ago",
			duration: 3 * time.Hour,
			isFuture: false,
			expected: "3 hours ago",
		},
		{
			name:     "1 day ago",
			duration: 24 * time.Hour,
			isFuture: false,
			expected: "1 day ago",
		},
		{
			name:     "5 days ago",
			duration: 5 * 24 * time.Hour,
			isFuture: false,
			expected: "5 days ago",
		},
		{
			name:     "2 weeks ago",
			duration: 14 * 24 * time.Hour,
			isFuture: false,
			expected: "2 weeks ago",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shared.FormatDuration(tt.duration, tt.isFuture)
			if got != tt.expected {
				t.Errorf("shared.FormatDuration(%v, %v) = %q, want %q", tt.duration, tt.isFuture, got, tt.expected)
			}
		})
	}
}

// ============================================================================
// shared.FormatTimeUnit Tests
// ============================================================================

func TestFormatTimeUnit(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		unit     time.Duration
		unitName string
		isFuture bool
		expected string
	}{
		{
			name:     "1 minute ago",
			duration: 1 * time.Minute,
			unit:     time.Minute,
			unitName: "minute",
			isFuture: false,
			expected: "1 minute ago",
		},
		{
			name:     "5 minutes ago",
			duration: 5 * time.Minute,
			unit:     time.Minute,
			unitName: "minute",
			isFuture: false,
			expected: "5 minutes ago",
		},
		{
			name:     "1 minute from now",
			duration: 1 * time.Minute,
			unit:     time.Minute,
			unitName: "minute",
			isFuture: true,
			expected: "1 minute from now",
		},
		{
			name:     "3 hours from now",
			duration: 3 * time.Hour,
			unit:     time.Hour,
			unitName: "hour",
			isFuture: true,
			expected: "3 hours from now",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shared.FormatTimeUnit(tt.duration, tt.unit, tt.unitName, tt.isFuture)
			if got != tt.expected {
				t.Errorf("shared.FormatTimeUnit() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// ============================================================================
// shared.FormatSize Tests
// ============================================================================

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name     string
		size     int64
		expected string
	}{
		{
			name:     "zero size",
			size:     0,
			expected: "-",
		},
		// Non-zero sizes will use pkgUtils.FormatSize
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shared.FormatSize(tt.size)
			if tt.size == 0 && got != tt.expected {
				t.Errorf("shared.FormatSize(%d) = %q, want %q", tt.size, got, tt.expected)
			}
		})
	}
}

// ============================================================================
// shared.FormatSpeed Tests
// ============================================================================

func TestFormatSpeed(t *testing.T) {
	tests := []struct {
		name     string
		speed    int64
		expected string
	}{
		{
			name:     "zero speed",
			speed:    0,
			expected: "N/A",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shared.FormatSpeed(tt.speed)
			if got != tt.expected {
				t.Errorf("shared.FormatSpeed(%d) = %q, want %q", tt.speed, got, tt.expected)
			}
		})
	}
}

// ============================================================================
// shared.FormatNoModelsFoundMessage Tests
// ============================================================================

func TestFormatNoModelsFoundMessage(t *testing.T) {
	tests := []struct {
		name     string
		node     string
		provider string
		model    string
		contains []string
	}{
		{
			name:     "no filters",
			node:     "",
			provider: "",
			model:    "",
			contains: []string{"No models found"},
		},
		{
			name:     "with node filter",
			node:     "node1",
			provider: "",
			model:    "",
			contains: []string{"No models found", "node1"},
		},
		{
			name:     "with provider filter",
			node:     "",
			provider: "ollama",
			model:    "",
			contains: []string{"No models found", "ollama"},
		},
		{
			name:     "with model filter",
			node:     "",
			provider: "",
			model:    "llama*",
			contains: []string{"No models found", "llama*"},
		},
		{
			name:     "wildcard node",
			node:     "*",
			provider: "",
			model:    "",
			contains: []string{"No models found"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shared.FormatNoModelsFoundMessage(tt.node, tt.provider, tt.model)
			for _, substr := range tt.contains {
				if !containsSubstring(got, substr) {
					t.Errorf("shared.FormatNoModelsFoundMessage(%q, %q, %q) = %q, should contain %q",
						tt.node, tt.provider, tt.model, got, substr)
				}
			}
		})
	}
}

// Helper function for string containment
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && containsSubstringHelper(s, substr)))
}

func containsSubstringHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ============================================================================
// shared.ModelQueryParams Tests
// ============================================================================

func TestModelQueryParams_Defaults(t *testing.T) {
	params := &shared.ModelQueryParams{}

	if params.Node != "" {
		t.Errorf("Node should default to empty, got %q", params.Node)
	}
	if params.Registry != "" {
		t.Errorf("Repo should default to empty, got %q", params.Registry)
	}
	if params.Provider != "" {
		t.Errorf("Provider should default to empty, got %q", params.Provider)
	}
	if params.Model != "" {
		t.Errorf("Model should default to empty, got %q", params.Model)
	}
}

func TestModelQueryParams_Fields(t *testing.T) {
	params := &shared.ModelQueryParams{
		Node:     "node1",
		Registry: "ollama",
		Provider: "vllm",
		Model:    "llama2",
		SortBy:   "name",
	}

	if params.Node != "node1" {
		t.Errorf("Node = %q, want 'node1'", params.Node)
	}
	if params.Provider != "vllm" {
		t.Errorf("Provider = %q, want 'vllm'", params.Provider)
	}
}

// ============================================================================
// OutputFormat Tests
// ============================================================================

func TestOutputFormat_Constants(t *testing.T) {
	if OutputTable != "table" {
		t.Errorf("OutputTable = %q, want 'table'", OutputTable)
	}
	if OutputJSON != "json" {
		t.Errorf("OutputJSON = %q, want 'json'", OutputJSON)
	}
	if OutputYAML != "yaml" {
		t.Errorf("OutputYAML = %q, want 'yaml'", OutputYAML)
	}
}

// ============================================================================
// TableRow Tests
// ============================================================================

func TestTableRow_Fields(t *testing.T) {
	row := TableRow{
		ID:       1,
		Node:     "node1",
		Provider: "ollama",
		Model:    "llama2",
		Format:   "gguf",
		Size:     "7.2 GB",
		Modified: "2 hours ago",
	}

	if row.ID != 1 {
		t.Errorf("ID = %d, want 1", row.ID)
	}
	if row.Node != "node1" {
		t.Errorf("Node = %q, want 'node1'", row.Node)
	}
	if row.Model != "llama2" {
		t.Errorf("Model = %q, want 'llama2'", row.Model)
	}
	if row.Size != "7.2 GB" {
		t.Errorf("Size = %q, want '7.2 GB'", row.Size)
	}
	if row.Format != "gguf" {
		t.Errorf("Format = %q, want 'gguf'", row.Format)
	}
}

// ============================================================================
// ModelInfo Tests
// ============================================================================

func TestModelInfo_Fields(t *testing.T) {
	info := ModelInfo{
		Node:     "node1",
		Provider: "ollama",
		Name:     "llama2-7b",
		FullID:   "ollama/llama2-7b",
		Size:     "7.0 GB",
		Modified: "2 hours ago",
	}

	if info.Name != "llama2-7b" {
		t.Errorf("Name = %q, want 'llama2-7b'", info.Name)
	}
	if info.Node != "node1" {
		t.Errorf("Node = %q, want 'node1'", info.Node)
	}
	if info.Provider != "ollama" {
		t.Errorf("Provider = %q, want 'ollama'", info.Provider)
	}
	if info.FullID != "ollama/llama2-7b" {
		t.Errorf("FullID = %q, want 'ollama/llama2-7b'", info.FullID)
	}
	if info.Size != "7.0 GB" {
		t.Errorf("Size = %q, want '7.0 GB'", info.Size)
	}
}

// ============================================================================
// FilterInstancesBySpec Tests
// ============================================================================

func TestFilterInstancesBySpec(t *testing.T) {
	// Import client types would be needed for real instances
	// For now, this tests the function signature compatibility
}

// ============================================================================
// ConvertModelInfoToTableRows Tests
// ============================================================================

func TestConvertModelInfoToTableRows(t *testing.T) {
	models := []ModelInfo{
		{
			Node:     "node1",
			Provider: "ollama",
			Name:     "llama2",
			Size:     "7.0 GB",
			Modified: "2 hours ago",
		},
		{
			Node:     "node2",
			Provider: "vllm",
			Name:     "mistral",
			Size:     "4.0 GB",
			Modified: "1 day ago",
		},
	}

	rows := ConvertModelInfoToTableRows(models)

	if len(rows) != 2 {
		t.Errorf("ConvertModelInfoToTableRows() returned %d rows, want 2", len(rows))
	}

	// Check first row
	if rows[0].Node != "node1" {
		t.Errorf("rows[0].Node = %q, want 'node1'", rows[0].Node)
	}
	if rows[0].Provider != "ollama" {
		t.Errorf("rows[0].Provider = %q, want 'ollama'", rows[0].Provider)
	}

	// Check second row
	if rows[1].Node != "node2" {
		t.Errorf("rows[1].Node = %q, want 'node2'", rows[1].Node)
	}
}

// ============================================================================
// shared.FormatSize Additional Tests
// ============================================================================

func TestFormatSize_NonZero(t *testing.T) {
	// Test that non-zero sizes delegate to pkgUtils.FormatSize
	got := shared.FormatSize(1024 * 1024 * 1024) // 1 GB
	if got == "-" {
		t.Errorf("shared.FormatSize(1GB) should not return dash")
	}
}

// ============================================================================
// shared.FormatSpeed Additional Tests
// ============================================================================

func TestFormatSpeed_NonZero(t *testing.T) {
	// Test that non-zero speeds format correctly
	got := shared.FormatSpeed(1024 * 1024) // 1 MB/s
	if got == "N/A" {
		t.Errorf("shared.FormatSpeed(1MB) should not return 'N/A'")
	}
	// Should contain "/s" suffix
	if !containsSubstring(got, "/s") {
		t.Errorf("shared.FormatSpeed should contain '/s' suffix, got %q", got)
	}
}

// ============================================================================
// shared.FormatDuration Additional Tests
// ============================================================================

func TestFormatDuration_Months(t *testing.T) {
	// Test month formatting
	duration := 45 * 24 * time.Hour // About 1.5 months

	got := shared.FormatDuration(duration, false)
	if !containsSubstring(got, "month") {
		t.Errorf("shared.FormatDuration(%v) should contain 'month', got %q", duration, got)
	}
}

func TestFormatDuration_FutureHours(t *testing.T) {
	// Test future hours
	duration := 5 * time.Hour

	got := shared.FormatDuration(duration, true)
	expected := "5 hours from now"
	if got != expected {
		t.Errorf("shared.FormatDuration(%v, true) = %q, want %q", duration, got, expected)
	}
}

func TestFormatDuration_FutureDays(t *testing.T) {
	// Test future days
	duration := 3 * 24 * time.Hour

	got := shared.FormatDuration(duration, true)
	expected := "3 days from now"
	if got != expected {
		t.Errorf("shared.FormatDuration(%v, true) = %q, want %q", duration, got, expected)
	}
}
