package search

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenRouterPageDescription(t *testing.T) {
	page, err := url.Parse("https://openrouter.ai/example/model:free")
	require.NoError(t, err)
	const full = "This model supports text and image input. See [usage](https://example.com/usage)."
	scoped := `<script type="application/ld+json">{"@type":"SoftwareApplication","url":"https://openrouter.ai/example/model%3Afree","description":"` + full + `"}</script>`
	unrelated := `<script type="application/ld+json">{"@type":"SoftwareApplication","url":"https://openrouter.ai/example/model","description":"This model supports other capabilities."}</script>`
	assert.Equal(t, full, openRouterPageDescription(strings.NewReader(unrelated+scoped), page, "This model supports"))
	assert.Empty(t, openRouterPageDescription(strings.NewReader(unrelated), page, "This model supports"))
	assert.Empty(t, openRouterPageDescription(strings.NewReader(scoped), page, "A different model"))
}

func TestOpenRouterDescriptionKeepsPreviewForCustomEndpoints(t *testing.T) {
	const preview = "This model supports..."
	assert.Equal(t, preview, expandOpenRouterDescription(t.Context(), "https://relay.example/api", "example/model:free", preview))
	assert.Equal(t, preview, expandOpenRouterDescription(t.Context(), "https://openrouter.ai/api", "../model", preview))
}
