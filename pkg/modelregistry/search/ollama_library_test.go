package search

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// loadLibraryFixture returns the captured ollama.com/library page. The
// fixture is real markup taken off the wire, not markup we invented:
// synthetic HTML only ever proves the parser agrees with the test author,
// which is how a parser can pass every unit test while returning an empty
// catalog in production.
func loadLibraryFixture(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "ollama_library.html"))
	if err != nil {
		t.Fatalf("read library fixture: %v", err)
	}
	return string(body)
}

func TestExtractModelsFromLibraryHTML_ReadsEveryCardOnTheCapturedPage(t *testing.T) {
	models := extractModelsFromLibraryHTML(loadLibraryFixture(t))

	byName := make(map[string]OllamaModelInfo, len(models))
	for _, m := range models {
		byName[m.Name] = m
	}
	if len(byName) != len(models) {
		t.Fatalf("duplicate model names in %d parsed cards", len(models))
	}

	tests := []struct {
		name     string
		pulls    int
		tags     int
		updated  string
		hasCloud bool
	}{
		{name: "qwen2.5", pulls: 38_700_000, tags: 133, updated: "1 year ago"},
		{name: "gpt-oss", pulls: 12_300_000, tags: 5, updated: "10 months ago", hasCloud: true},
		{name: "glm-5.1", pulls: 2_300_000, tags: 0, updated: "4 months ago", hasCloud: true},
		{name: "nomic-embed-text", pulls: 83_700_000, tags: 3, updated: "2 years ago"},
		{name: "nomic-embed-text-v2-moe", pulls: 803_500, tags: 0, updated: "8 months ago"},
		{name: "llama3.1", pulls: 118_900_000, tags: 93, updated: "1 year ago"},
	}
	if len(models) != len(tests) {
		t.Fatalf("parsed %d models, fixture holds %d cards", len(models), len(tests))
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := byName[tt.name]
			if !ok {
				t.Fatalf("model %q missing from the parsed catalog", tt.name)
			}
			if got.Pulls != tt.pulls {
				t.Errorf("pulls = %d, want %d", got.Pulls, tt.pulls)
			}
			if got.Tags != tt.tags {
				t.Errorf("tags = %d, want %d", got.Tags, tt.tags)
			}
			if got.ModifiedAt != tt.updated {
				t.Errorf("updated = %q, want %q", got.ModifiedAt, tt.updated)
			}
			if got.HasCloud != tt.hasCloud {
				t.Errorf("has_cloud = %v, want %v", got.HasCloud, tt.hasCloud)
			}
			if got.URL != "https://ollama.com/library/"+tt.name {
				t.Errorf("url = %q, want the model's library page", got.URL)
			}
		})
	}
}

// A model name can be a prefix of another's, so a card has to be found by
// its own link and not by the first place its name appears. The two real
// cards are re-ordered here to put the longer name first: ollama.com
// orders the page by popularity, so which of a prefix pair comes first is
// not something this parser gets to assume.
func TestExtractLibraryCards_ReadsEachCardFromItsOwnLink(t *testing.T) {
	const prefix, longer = "nomic-embed-text", "nomic-embed-text-v2-moe"

	fixture := make(map[string]string)
	for _, c := range extractLibraryCards(loadLibraryFixture(t)) {
		fixture[c.name] = c.section
	}
	for _, name := range []string{prefix, longer} {
		if fixture[name] == "" {
			t.Fatalf("no card for %s in the fixture", name)
		}
	}

	cards := extractLibraryCards(fixture[longer] + fixture[prefix])
	got := make(map[string]int, len(cards))
	for _, c := range cards {
		got[c.name] = extractLibraryStat(c.section, "Pulls")
	}

	// Each card's own count, read off the captured page.
	want := map[string]int{longer: 803_500, prefix: 83_700_000}
	for name, pulls := range want {
		if got[name] != pulls {
			t.Errorf("%s pulls = %d, want its own card's %d", name, got[name], pulls)
		}
	}
}

func TestExtractLibraryStat_DistinguishesTheLabels(t *testing.T) {
	// The two stats are the same markup with a different label, so a
	// pattern that ignores the label reads whichever comes first.
	const section = `<span >38.7M</span>
		<span class="hidden sm:flex">&nbsp;Pulls</span>
		<span >133</span>
		<span class="hidden sm:flex">&nbsp;Tags</span>`

	if got := extractLibraryStat(section, "Pulls"); got != 38_700_000 {
		t.Errorf("Pulls = %d, want 38700000", got)
	}
	if got := extractLibraryStat(section, "Tags"); got != 133 {
		t.Errorf("Tags = %d, want 133", got)
	}
	if got := extractLibraryStat(section, "Pulls"); got == 133 {
		t.Error("Pulls read the tag count")
	}
}

func TestParseLibraryPage_RefusesAPageThatHeldNoCards(t *testing.T) {
	_, err := parseLibraryPage([]byte(`<html><body><a href="/pricing">Pricing</a></body></html>`))
	if err == nil {
		t.Fatal("a page with no model cards parsed as a successful empty catalog")
	}
	if !errors.Is(err, ErrLibraryUnparsed) {
		t.Errorf("err = %v, want it to wrap ErrLibraryUnparsed", err)
	}
}

func TestLocallyRunnable_KeepsCloudModelsThatStillShipTags(t *testing.T) {
	all := []OllamaModelInfo{
		{Name: "qwen2.5", Tags: 133},
		{Name: "gpt-oss", Tags: 5, HasCloud: true},
		{Name: "glm-5.1", HasCloud: true},
		{Name: "nomic-embed-text-v2-moe"},
	}

	var got []string
	for _, m := range locallyRunnable(all) {
		got = append(got, m.Name)
	}

	want := []string{"qwen2.5", "gpt-oss", "nomic-embed-text-v2-moe"}
	if len(got) != len(want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kept %v, want %v", got, want)
		}
	}
}
