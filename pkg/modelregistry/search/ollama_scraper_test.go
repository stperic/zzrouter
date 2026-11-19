package search

import (
	"slices"
	"testing"
	"time"
)

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

func TestOllamaTagMetadataUsesTheExactTagCard(t *testing.T) {
	const html = `
<a href="/library/autotrust-jev:latest"><span>autotrust-jev:latest</span>
  <span>abcdefabcdef</span> • 11GB • 256K context window • <span>Text input • 1 week ago</span>
</a>
<a href="/library/autotrust-jev:latest">autotrust-jev:latest</a>
<a href="/library/autotrust-jev:9b-mxfp8"><span>autotrust-jev:9b-mxfp8</span><span class="badge">MLX</span>
  <span>abcdefabcdef</span> • 11GB • 256K context window • <span>Text input • 1 week ago</span>
</a>
<a href="/library/autotrust-jev:35b"><span>autotrust-jev:35b</span>
  <span>012345abcdef</span> • 26GB • 256K context window • <span>Text, Image input • 1 week ago</span>
</a>
<a href="/library/autotrust-jev:unreported">Unreported</a>
`
	got := extractTagDetailsFromHTML(html)
	want := []OllamaTagInfo{
		{Name: "latest", Size: "11GB", Context: "256K", Input: "Text"},
		{Name: "9b-mxfp8", Size: "11GB", Context: "256K", Input: "Text", IsMLX: true},
		{Name: "35b", Size: "26GB", Context: "256K", Input: "Text, Image"},
		{Name: "unreported", Size: "-", Context: "-"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parsed tags = %+v, want %+v", got, want)
	}
}

func TestOllamaLibraryCapabilitiesAreScopedToEachCard(t *testing.T) {
	const markup = `
<a href="/library/ripple-preview">
  <span class="inline-flex items-center rounded-md bg-indigo-50">tools</span>
  <span class="inline-flex items-center rounded-md bg-indigo-50">thinking</span>
  <span class="inline-flex items-center rounded-md bg-indigo-50">tools</span>
  <p class="max-w-lg break-words text-neutral-800 text-md">Code &amp; reasoning.</p>
  <span class="inline-flex rounded-md bg-[#ddf4ff]">65b</span>
  <span>6.8M</span><span>&nbsp;Pulls</span>
  <span class="flex items-center" title="Nov 9, 2026 11:18 PM UTC"><span>Updated&nbsp;</span><span>1 week ago</span></span>
</a>
<a href="/library/autotrust-jev">
  <span class="inline-flex items-center rounded-md bg-indigo-50">vision</span>
  <span class="inline-flex items-center rounded-md bg-indigo-50">decision</span>
  <span>11</span><span>&nbsp;Tags</span>
  <span title="Nov 4, 2026 11:26 PM UTC"><span>Updated&nbsp;</span><span>1 week ago</span></span>
</a>
`
	got, err := parseLibraryPage([]byte(markup))
	if err != nil || len(got) != 2 {
		t.Fatalf("parsed models = %+v, err = %v", got, err)
	}
	if !slices.Equal(got[0].Capabilities, []string{"tools", "thinking"}) ||
		!slices.Equal(got[1].Capabilities, []string{"vision", "decision"}) || got[1].Tags != 11 {
		t.Fatalf("parsed capabilities and tag counts = %+v", got)
	}
	if got[0].Description != "Code & reasoning." || !slices.Equal(got[0].ParameterSizes, []string{"65b"}) {
		t.Fatalf("parsed description and sizes = %+v", got[0])
	}
	if want := time.Date(2026, time.November, 9, 23, 18, 0, 0, time.UTC); !got[0].UpdatedAt.Equal(want) {
		t.Fatalf("update time = %s, want %s", got[0].UpdatedAt, want)
	}
	got[0], got[1] = got[1], got[0]
	sortOllamaModels(got, "modified", -1)
	if got[0].Name != "ripple-preview" {
		t.Fatalf("exact timestamps did not order models with equal relative dates: %+v", got)
	}
}
