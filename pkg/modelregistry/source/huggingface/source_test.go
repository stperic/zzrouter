package huggingface

import (
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
