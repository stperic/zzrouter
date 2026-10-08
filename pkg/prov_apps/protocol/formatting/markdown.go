package formatting

import (
	"strings"
)

// RenderMarkdown renders TableData as a Markdown table.
// This is used by slash commands for chat UI output.
func RenderMarkdown(t *TableData) string {
	if t.IsEmpty() {
		if t.Empty != "" {
			return t.Empty
		}
		return "No data available."
	}

	var sb strings.Builder

	// Render title if present (as heading)
	if t.Title != "" {
		sb.WriteString("## ")
		sb.WriteString(t.Title)
		sb.WriteString("\n\n")
	}

	// Render header row
	sb.WriteString("|")
	for _, col := range t.Columns {
		sb.WriteString(" ")
		sb.WriteString(col.Header)
		sb.WriteString(" |")
	}
	sb.WriteString("\n")

	// Render separator row with alignment
	sb.WriteString("|")
	for _, col := range t.Columns {
		sb.WriteString(markdownAlignmentSeparator(col.Align))
		sb.WriteString("|")
	}
	sb.WriteString("\n")

	// Render data rows
	for _, row := range t.Rows {
		sb.WriteString("|")
		for i := range t.Columns {
			sb.WriteString(" ")
			if i < len(row.Cells) {
				// Escape pipe characters in values
				value := strings.ReplaceAll(row.Cells[i].Value, "|", "\\|")
				sb.WriteString(value)
			}
			sb.WriteString(" |")
		}
		sb.WriteString("\n")
	}

	// Render footer if present
	if t.Footer != "" {
		sb.WriteString("\n")
		sb.WriteString(t.Footer)
	}

	return sb.String()
}

// markdownAlignmentSeparator returns the Markdown separator for a column alignment.
func markdownAlignmentSeparator(align Align) string {
	switch align {
	case AlignRight:
		return "---:"
	case AlignCenter:
		return ":---:"
	default: // AlignLeft
		return "---"
	}
}

// RenderMarkdownKeyValue renders a key-value list as Markdown.
// Useful for detail views (e.g., /show command).
func RenderMarkdownKeyValue(title string, pairs []KeyValue) string {
	var sb strings.Builder

	if title != "" {
		sb.WriteString("## ")
		sb.WriteString(title)
		sb.WriteString("\n\n")
	}

	// Render as a two-column table
	sb.WriteString("| Property | Value |\n")
	sb.WriteString("|----------|-------|\n")

	for _, kv := range pairs {
		if kv.Value != "" {
			value := strings.ReplaceAll(kv.Value, "|", "\\|")
			sb.WriteString("| ")
			sb.WriteString(kv.Key)
			sb.WriteString(" | ")
			sb.WriteString(value)
			sb.WriteString(" |\n")
		}
	}

	return sb.String()
}

// KeyValue represents a key-value pair for detail views.
type KeyValue struct {
	Key   string
	Value string
}

// RenderMarkdownList renders a simple list as Markdown bullet points.
func RenderMarkdownList(title string, items []string) string {
	if len(items) == 0 {
		return ""
	}

	var sb strings.Builder

	if title != "" {
		sb.WriteString("**")
		sb.WriteString(title)
		sb.WriteString("**\n\n")
	}

	for _, item := range items {
		sb.WriteString("- ")
		sb.WriteString(item)
		sb.WriteString("\n")
	}

	return sb.String()
}

// RenderMarkdownError formats an error message for Markdown display.
func RenderMarkdownError(message string) string {
	return "**Error:** " + message
}

// RenderMarkdownInlineCode wraps text in inline code.
func RenderMarkdownInlineCode(text string) string {
	return "`" + text + "`"
}

// RenderMarkdownBold wraps text in bold.
func RenderMarkdownBold(text string) string {
	return "**" + text + "**"
}
