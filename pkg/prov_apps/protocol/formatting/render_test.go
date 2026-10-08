package formatting

import (
	"strings"
	"testing"
)

func TestRenderMarkdown(t *testing.T) {
	t.Run("empty table", func(t *testing.T) {
		table := NewTableData(Col("Name"), Col("Value"))
		result := RenderMarkdown(table)
		if result != "No data available." {
			t.Errorf("RenderMarkdown empty = %q, want 'No data available.'", result)
		}
	})

	t.Run("simple table", func(t *testing.T) {
		table := NewTableData(Col("Name"), Col("Value"))
		table.AddRow("foo", "bar")

		result := RenderMarkdown(table)

		// Check markdown table format
		if !strings.Contains(result, "| Name | Value |") {
			t.Errorf("RenderMarkdown should contain header row, got: %q", result)
		}
		if !strings.Contains(result, "|---|---|") {
			t.Errorf("RenderMarkdown should contain separator row")
		}
		if !strings.Contains(result, "| foo | bar |") {
			t.Errorf("RenderMarkdown should contain data row")
		}
	})

	t.Run("with title", func(t *testing.T) {
		table := NewTableData(Col("Name"))
		table.Title = "Test Table"
		table.AddRow("item")

		result := RenderMarkdown(table)
		if !strings.Contains(result, "## Test Table") {
			t.Errorf("RenderMarkdown should contain title as heading")
		}
	})

	t.Run("escapes pipe characters", func(t *testing.T) {
		table := NewTableData(Col("Command"))
		table.AddRow("echo foo | grep bar")

		result := RenderMarkdown(table)
		if !strings.Contains(result, "echo foo \\| grep bar") {
			t.Errorf("RenderMarkdown should escape pipe characters, got: %q", result)
		}
	})

	t.Run("alignment", func(t *testing.T) {
		table := NewTableData(
			Col("Left"),
			ColRight("Right"),
		)
		table.AddRow("a", "b")

		result := RenderMarkdown(table)
		if !strings.Contains(result, "|---|") {
			t.Errorf("RenderMarkdown should have left-aligned column")
		}
		if !strings.Contains(result, "|---:|") {
			t.Errorf("RenderMarkdown should have right-aligned column")
		}
	})
}

func TestRenderMarkdownKeyValue(t *testing.T) {
	pairs := []KeyValue{
		{Key: "Name", Value: "test-model"},
		{Key: "Size", Value: "1.5 GB"},
		{Key: "Empty", Value: ""},
	}

	result := RenderMarkdownKeyValue("Model Details", pairs)

	// Check title
	if !strings.Contains(result, "## Model Details") {
		t.Errorf("Should contain title")
	}

	// Check property table
	if !strings.Contains(result, "| Property | Value |") {
		t.Errorf("Should contain property header")
	}

	// Check values (empty values should be skipped)
	if !strings.Contains(result, "| Name | test-model |") {
		t.Errorf("Should contain Name row")
	}
	if !strings.Contains(result, "| Size | 1.5 GB |") {
		t.Errorf("Should contain Size row")
	}
	if strings.Contains(result, "| Empty |") {
		t.Errorf("Should skip empty values")
	}
}

func TestRenderMarkdownList(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		result := RenderMarkdownList("Errors", []string{})
		if result != "" {
			t.Errorf("Empty list should return empty string")
		}
	})

	t.Run("with items", func(t *testing.T) {
		result := RenderMarkdownList("Errors", []string{"error 1", "error 2"})

		if !strings.Contains(result, "**Errors**") {
			t.Errorf("Should contain title")
		}
		if !strings.Contains(result, "- error 1") {
			t.Errorf("Should contain first item")
		}
		if !strings.Contains(result, "- error 2") {
			t.Errorf("Should contain second item")
		}
	})
}

func TestMarkdownHelpers(t *testing.T) {
	t.Run("RenderMarkdownError", func(t *testing.T) {
		result := RenderMarkdownError("something went wrong")
		if result != "**Error:** something went wrong" {
			t.Errorf("Unexpected error format: %q", result)
		}
	})

	t.Run("RenderMarkdownBold", func(t *testing.T) {
		result := RenderMarkdownBold("important")
		if result != "**important**" {
			t.Errorf("Unexpected bold format: %q", result)
		}
	})

	t.Run("RenderMarkdownInlineCode", func(t *testing.T) {
		result := RenderMarkdownInlineCode("code")
		if result != "`code`" {
			t.Errorf("Unexpected inline code format: %q", result)
		}
	})

}
