package search

import "testing"

// The relative date is the span that follows the "Updated" label, and
// the label carries a non-breaking space the date does not.
func TestExtractUpdatedFromSection(t *testing.T) {
	tests := []struct {
		name     string
		html     string
		expected string
	}{
		{
			name:     "Standard format",
			html:     `<span class="hidden sm:flex">Updated&nbsp;</span><span >1 month ago</span>`,
			expected: "1 month ago",
		},
		{
			name:     "With extra whitespace",
			html:     "<span>Updated&nbsp;</span>\n\t<span >  2 weeks ago  </span>",
			expected: "2 weeks ago",
		},
		{
			name:     "No match",
			html:     `<span>Some other text</span>`,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractUpdatedFromSection(tt.html)
			if result != tt.expected {
				t.Errorf("extractUpdatedFromSection() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// Test parseRelativeDate
func TestParseRelativeDate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{
			name:     "1 month ago",
			input:    "1 month ago",
			expected: 30,
		},
		{
			name:     "2 weeks ago",
			input:    "2 weeks ago",
			expected: 14,
		},
		{
			name:     "3 days ago",
			input:    "3 days ago",
			expected: 3,
		},
		{
			name:     "1 year ago",
			input:    "1 year ago",
			expected: 365,
		},
		{
			name:     "5 hours ago",
			input:    "5 hours ago",
			expected: 0,
		},
		{
			name:     "Empty string",
			input:    "",
			expected: 999999,
		},
		{
			name:     "Invalid format",
			input:    "yesterday",
			expected: 999999,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseRelativeDate(tt.input)
			if result != tt.expected {
				t.Errorf("parseRelativeDate(%q) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}

// The two models this check has always named are in the captured
// fixture, so a date that stops parsing fails here without a network.
func TestExtractUpdatedFromSection_OnCapturedCards(t *testing.T) {
	sections := make(map[string]string)
	for _, c := range extractLibraryCards(loadLibraryFixture(t)) {
		sections[c.name] = c.section
	}

	for name, want := range map[string]string{
		"qwen2.5":  "1 year ago",
		"llama3.1": "1 year ago",
	} {
		t.Run(name, func(t *testing.T) {
			section, ok := sections[name]
			if !ok {
				t.Fatalf("no card for %s in the fixture", name)
			}
			if got := extractUpdatedFromSection(section); got != want {
				t.Errorf("updated = %q, want %q", got, want)
			}
		})
	}
}
