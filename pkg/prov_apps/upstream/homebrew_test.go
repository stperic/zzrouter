package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

func homebrewSource() *config.VersionSource {
	return &config.VersionSource{
		Type:    config.VersionSourceHomebrew,
		Formula: "ollama",
		Compare: config.CompareSemver,
	}
}

// The formula's stable version is a packager's ceiling, which is the right
// answer for a node that installs through that packager.
func TestLatestHomebrew(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/formula/ollama.json", r.URL.Path)
		_, _ = w.Write([]byte(`{"name":"ollama","versions":{"stable":"0.32.14","head":"HEAD"},"revision":0}`))
	}))
	defer srv.Close()

	res := NewResolver()
	res.homebrewBase = srv.URL

	got, err := res.Latest(context.Background(), homebrewSource())
	require.NoError(t, err)

	// No prefix to strip: the formula names a bare version, so tag and
	// version coincide and either can be handed back to an install.
	assert.Equal(t, "0.32.14", got.Version)
	assert.Equal(t, "0.32.14", got.Tag)
	assert.Contains(t, got.URL, "/formula/ollama")
}

func TestLatestHomebrewErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name:    "no such formula",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			wantErr: "no formula",
		},
		{
			name:    "server error",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
			wantErr: "HTTP 502",
		},
		{
			name:    "unreadable body",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
			wantErr: "unreadable",
		},
		{
			// A formula with only a HEAD spec names no stable version. That
			// is "nothing to compare against", not "up to date".
			name: "head only",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"versions":{"stable":null,"head":"HEAD"}}`))
			},
			wantErr: "no stable version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			res := NewResolver()
			res.homebrewBase = srv.URL

			_, err := res.Latest(context.Background(), homebrewSource())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// Keying on the provider alone would let one platform's ceiling overwrite
// another's, and whichever was fetched last would answer for both.
func TestCacheKey(t *testing.T) {
	plain := &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "a/b"}
	split := &config.VersionSource{
		Type: config.VersionSourceGitHubRelease, Repo: "ollama/ollama",
		Platforms: map[string]*config.VersionSource{"darwin": homebrewSource()},
	}

	// A provider with one answer shares one entry across every node, which is
	// both correct and a single fetch.
	assert.Equal(t, "ollama", CacheKey("ollama", "linux", plain))
	assert.Equal(t, "ollama", CacheKey("ollama", "darwin", plain))
	assert.Equal(t, "ollama", CacheKey("ollama", "", split))

	// Only the platform that genuinely resolves differently gets its own.
	assert.NotEqual(t, CacheKey("ollama", "darwin", split), CacheKey("ollama", "linux", split))
	assert.Equal(t, "ollama", CacheKey("ollama", "linux", split),
		"a platform with no override shares the base entry")
}

// Invalidate is called because a provider changed, without knowing which
// platform's ceiling should be re-asked about.
func TestCacheInvalidateDropsEveryPlatform(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"versions":{"stable":"0.32.14"}}`))
	}))
	defer srv.Close()

	c := NewCache(0, 0)
	c.resolver = NewResolver()
	c.resolver.homebrewBase = srv.URL

	src := homebrewSource()
	c.Lookup(context.Background(), "ollama", src, true)
	c.Lookup(context.Background(), "ollama\x00darwin", src, true)
	_, baseOK := c.Peek("ollama")
	_, darwinOK := c.Peek("ollama\x00darwin")
	require.True(t, baseOK && darwinOK)

	c.Invalidate("ollama")
	_, baseOK = c.Peek("ollama")
	_, darwinOK = c.Peek("ollama\x00darwin")
	assert.False(t, baseOK)
	assert.False(t, darwinOK, "a per-platform entry must not survive its provider")
}
