package search

import (
	"testing"
)

func TestEstimateOllamaModelSize(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		expected  int64
	}{
		{"7b model", "llama2:7b", 4900000000},      // 7e9 * 0.7
		{"14b model", "codellama:14b", 9800000000}, // 14e9 * 0.7
		{"70b model", "llama2:70b", 49000000000},   // 70e9 * 0.7
		{"1.5b model", "llama3.2:1b", 700000000},   // Falls back to 1e9 * 0.7 (doesn't match "1b" pattern)
		{"no size info", "some-model", 4900000000}, // Default 7e9 * 0.7
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EstimateOllamaModelSize(tt.modelName)
			if result != tt.expected {
				t.Errorf("EstimateOllamaModelSize(%s) = %d, want %d", tt.modelName, result, tt.expected)
			}
		})
	}
}

func TestFilterOllamaModels(t *testing.T) {
	models := []OllamaModelInfo{
		{Name: "llama3.1"},
		{Name: "llama3.2"},
		{Name: "mistral"},
		{Name: "codellama"},
	}

	tests := []struct {
		name     string
		query    string
		expected []string
	}{
		{"empty query", "", []string{"llama3.1", "llama3.2", "mistral", "codellama"}},
		{"substring match", "llama", []string{"llama3.1", "llama3.2", "codellama"}},
		{"exact match", "mistral", []string{"mistral"}},
		{"wildcard match", "llama*", []string{"llama3.1", "llama3.2"}},
		{"no match", "nonexistent", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterOllamaModels(models, tt.query)
			if len(result) != len(tt.expected) {
				t.Errorf("filterOllamaModels(%s) returned %d results, want %d", tt.query, len(result), len(tt.expected))
				return
			}
			for i, expected := range tt.expected {
				if result[i].Name != expected {
					t.Errorf("filterOllamaModels(%s)[%d] = %s, want %s", tt.query, i, result[i].Name, expected)
				}
			}
		})
	}
}

func TestSortOllamaModels(t *testing.T) {
	models := []OllamaModelInfo{
		{Name: "small-model", Size: 1000000000},
		{Name: "large-model", Size: 50000000000},
		{Name: "medium-model", Size: 10000000000},
	}

	tests := []struct {
		name      string
		sortBy    string
		direction int
		expected  []string
	}{
		{"size ascending", "size", 1, []string{"small-model", "medium-model", "large-model"}},
		{"size descending", "size", -1, []string{"large-model", "medium-model", "small-model"}},
		{"default sort", "", -1, []string{"large-model", "medium-model", "small-model"}}, // defaults to size desc
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sortOllamaModels(models, tt.sortBy, tt.direction)
			for i, expected := range tt.expected {
				if result[i].Name != expected {
					t.Errorf("sortOllamaModels()[%d] = %s, want %s", i, result[i].Name, expected)
				}
			}
		})
	}
}

func TestPaginateOllamaModels(t *testing.T) {
	models := []OllamaModelInfo{
		{Name: "model1"},
		{Name: "model2"},
		{Name: "model3"},
		{Name: "model4"},
		{Name: "model5"},
	}

	pagination := PaginationParams{
		Offset: 1,
		Limit:  2,
	}

	result := paginateOllamaModels(models, pagination)

	if result.Total != 5 {
		t.Errorf("Total = %d, want 5", result.Total)
	}
	if len(result.Results) != 2 {
		t.Errorf("Results length = %d, want 2", len(result.Results))
	}
	if result.Results[0].Name != "model2" {
		t.Errorf("First result = %s, want model2", result.Results[0].Name)
	}
	if !result.HasMore {
		t.Errorf("HasMore = false, want true")
	}
}
