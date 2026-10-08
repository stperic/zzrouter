package search

import "testing"

// The card abbreviates its counts, so the suffix carries most of the
// magnitude. Each case below is the markup shape ollama.com serves: the
// number in one span, its label in the next.
func TestExtractLibraryStat_ExpandsAbbreviatedCounts(t *testing.T) {
	tests := []struct {
		name     string
		number   string
		expected int
	}{
		{name: "millions", number: "11.7M", expected: 11_700_000},
		{name: "thousands", number: "523.4K", expected: 523_400},
		{name: "billions", number: "1.2B", expected: 1_200_000_000},
		{name: "no multiplier", number: "999", expected: 999},
		{name: "number with comma", number: "5,478", expected: 5478},
		{name: "large number with commas", number: "1,234,567", expected: 1_234_567},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			section := `<span >` + tt.number + `</span><span class="hidden sm:flex">&nbsp;Pulls</span>`
			if result := extractLibraryStat(section, "Pulls"); result != tt.expected {
				t.Errorf("extractLibraryStat() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestExtractLibraryStat_ReportsZeroForAnAbsentStat(t *testing.T) {
	if result := extractLibraryStat("Some random text", "Pulls"); result != 0 {
		t.Errorf("extractLibraryStat() = %d, want 0", result)
	}
}
