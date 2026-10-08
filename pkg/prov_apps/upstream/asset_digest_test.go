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

// testGitHubSource is the llama.cpp feed shape: a GitHub release source whose
// tags carry a "b" prefix.
func testGitHubSource() *config.VersionSource {
	return &config.VersionSource{
		Type:        config.VersionSourceGitHubRelease,
		Repo:        "ggml-org/llama.cpp",
		StripPrefix: "b",
	}
}

const releaseWithDigests = `{"assets":[
  {"name":"llama-b10549-bin-ubuntu-vulkan-x64.tar.gz","digest":"sha256:AABBCCDD"},
  {"name":"llama-b10549-bin-macos-arm64.tar.gz","digest":""},
  {"name":"llama-b10549-bin-ubuntu-x64.tar.gz","digest":"md5:deadbeef"}
]}`

// digestResolver points a Resolver at a stub API and reports the paths it saw.
func digestResolver(t *testing.T, status int, body string) (*Resolver, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	r := NewResolver()
	r.githubAPIBase = srv.URL
	return r, &seen
}

func TestAssetDigest(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{
			name:     "returns the recorded hash in lowercase",
			filename: "llama-b10549-bin-ubuntu-vulkan-x64.tar.gz",
			want:     "aabbccdd",
		},
		{
			// Absent is not an error: the caller degrades to a warning, and
			// conflating "no digest published" with "lookup failed" would
			// make both render the same way.
			name:     "empty digest reads as absent",
			filename: "llama-b10549-bin-macos-arm64.tar.gz",
			want:     "",
		},
		{
			// VerifySHA256 is what consumes this. Handing it an MD5 would
			// compare the wrong hash and fail an install for a good download.
			name:     "non-sha256 algorithm reads as absent",
			filename: "llama-b10549-bin-ubuntu-x64.tar.gz",
			want:     "",
		},
		{
			name:     "unknown asset reads as absent",
			filename: "not-in-this-release.tar.gz",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := digestResolver(t, http.StatusOK, releaseWithDigests)
			got, err := r.AssetDigest(context.Background(), testGitHubSource(), "b10549", tt.filename)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAssetDigest_LooksUpTheReleaseByTag(t *testing.T) {
	r, seen := digestResolver(t, http.StatusOK, releaseWithDigests)

	_, err := r.AssetDigest(context.Background(), testGitHubSource(), "b10549", "x.tar.gz")
	require.NoError(t, err)
	require.Len(t, *seen, 1)
	assert.Equal(t, "/repos/ggml-org/llama.cpp/releases/tags/b10549", (*seen)[0])
}

// A rate-limited or unreachable API must be an error, distinct from a release
// that simply lists no digest: the caller warns and installs anyway in both
// cases, but only one of them should be reported as a lookup that failed.
func TestAssetDigest_APIFailureIsAnError(t *testing.T) {
	r, _ := digestResolver(t, http.StatusForbidden, `{"message":"API rate limit exceeded"}`)

	_, err := r.AssetDigest(context.Background(), testGitHubSource(), "b10549", "x.tar.gz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestAssetDigest_RejectsMissingOperands(t *testing.T) {
	r, seen := digestResolver(t, http.StatusOK, releaseWithDigests)

	_, err := r.AssetDigest(context.Background(), testGitHubSource(), "", "x.tar.gz")
	require.Error(t, err, "no tag means no release to look up")

	_, err = r.AssetDigest(context.Background(), nil, "b10549", "x.tar.gz")
	require.Error(t, err, "no source means no repo to look up")

	assert.Empty(t, *seen, "neither case should reach the network")
}
