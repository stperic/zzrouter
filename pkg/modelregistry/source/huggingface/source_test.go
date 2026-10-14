package huggingface

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseProviderModelSource(t *testing.T) {
	for _, source := range []string{"org/repo#nested/a b.safetensors", "https://huggingface.co/org/repo/resolve/main/nested/a%20b.safetensors"} {
		r, err := ParseSource(source)
		require.NoError(t, err)
		require.Equal(t, "org/repo", r.Repo)
		require.Equal(t, "main", r.Revision)
		require.Equal(t, "nested/a b.safetensors", r.Weights)
	}
	r, err := ParseSource("https://huggingface.co/org/repo/resolve/refs%2Fpr%2F7/a%2520b.safetensors")
	require.NoError(t, err)
	require.Equal(t, "refs/pr/7", r.Revision)
	require.Equal(t, "a%20b.safetensors", r.Weights, "decode a URL segment once")
	for _, source := range []string{"", "org/repo", "org/repo#../file", "org/repo#", "org/repo/more#file", "org/../repo#file", "https://example.com/org/repo/resolve/main/file", "http://huggingface.co/org/repo/resolve/main/file", "https://huggingface.co:443/org/repo/resolve/main/file", "https://user:pass@huggingface.co/org/repo/resolve/main/file", "https://huggingface.co/org/repo/blob/main/file", "https://huggingface.co/org/repo/resolve/main/%2E%2E/file", "https://huggingface.co/org/repo/resolve/main/file?token=ignored", "https://huggingface.co/org/repo/resolve/main/file#part", "https://huggingface.co/org/repo/resolve//file"} {
		t.Run(source, func(t *testing.T) { _, err := ParseSource(source); require.Error(t, err) })
	}
}

func TestParseProviderModelSourceInertDownloadQuery(t *testing.T) {
	const source = "https://huggingface.co/Comfy-Org/flux1-schnell/resolve/main/flux1-schnell-fp8.safetensors?download=true"
	got, err := ParseSource(source)
	require.NoError(t, err)
	want, err := ParseSource(strings.TrimSuffix(source, "?download=true"))
	require.NoError(t, err)
	require.Equal(t, want, got, "discard the inert query from source identity and materialization")
	require.Equal(t, "Comfy-Org/flux1-schnell", got.Repo)
	require.Equal(t, "main", got.Revision)
	require.Equal(t, "flux1-schnell-fp8.safetensors", got.Weights)
	for _, query := range []string{"", "download=false", "download=True", "download=true&download=true", "download=true&token=secret", "token=secret&download=true", "download=%74rue", "%64ownload=true", "download=true&", "download=true;other=value"} {
		t.Run(query, func(t *testing.T) {
			_, err := ParseSource(strings.TrimSuffix(source, "download=true") + query)
			require.Error(t, err)
		})
	}
}
