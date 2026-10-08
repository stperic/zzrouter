package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

// stubResolver returns a Resolver pointed at a stub server. Each test owns
// its own, so nothing is shared and nothing needs restoring.
func stubResolver(url string) *Resolver {
	r := NewResolver()
	r.githubBase = url
	r.pypiBase = url
	return r
}

func TestLatestGitHubRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/ggml-org/llama.cpp/releases/latest", r.URL.Path)
		w.Header().Set("Location", "/ggml-org/llama.cpp/releases/tag/b10502")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	res := stubResolver(srv.URL)

	got, err := res.Latest(context.Background(), buildNumberSource())
	require.NoError(t, err)

	assert.Equal(t, "b10502", got.Tag)
	// strip_prefix is applied to the reported version but not the tag: an
	// install requests "b10502", a comparison uses 10502.
	assert.Equal(t, "10502", got.Version)
	assert.Contains(t, got.URL, "releases/tag/b10502")
}

func TestLatestGitHubReleaseErrors(t *testing.T) {
	t.Run("no latest release", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		res := stubResolver(srv.URL)

		_, err := res.Latest(context.Background(), buildNumberSource())
		require.ErrorIs(t, err, ErrNoUpstreamRelease)
	})

	// A repo that has published nothing redirects to the bare .../releases
	// listing. Taking the last path segment there yields the literal
	// "releases", which would sail through as a version and become both a
	// bogus install target and a bogus "update available".
	t.Run("repo with no releases at all", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/ggml-org/llama.cpp/releases")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		res := stubResolver(srv.URL)

		got, err := res.Latest(context.Background(), buildNumberSource())
		require.ErrorIs(t, err, ErrNoUpstreamRelease)
		assert.Empty(t, got.Tag, "must not report %q as a release", got.Tag)
	})

	// A redirect that leaves the origin must never contribute a tag: the tag
	// flows into a download URL.
	t.Run("off-host redirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://evil.example/ggml-org/llama.cpp/releases/tag/b99999")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		res := stubResolver(srv.URL)

		got, err := res.Latest(context.Background(), buildNumberSource())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "off-host")
		assert.Empty(t, got.Tag)
	})

	// A tag from another repo on the same host must not be adopted either.
	t.Run("redirect to a different repo", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/someone/else/releases/tag/b99999")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		res := stubResolver(srv.URL)

		got, err := res.Latest(context.Background(), buildNumberSource())
		require.ErrorIs(t, err, ErrNoUpstreamRelease)
		assert.Empty(t, got.Tag)
	})

	t.Run("unusable redirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/ggml-org/llama.cpp/releases/tag/")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		res := stubResolver(srv.URL)

		_, err := res.Latest(context.Background(), buildNumberSource())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unusable tag")
	})
}

// simpleIndexJSON mirrors the shape PyPI serves for the PEP 691 Accept
// header, including the tri-state yanked field.
const simpleIndexJSON = `{
  "meta": {"api-version": "1.4"},
  "versions": ["0.2.1", "0.9.2", "0.19.0", "0.27.1"],
  "files": [
    {"filename": "vllm-0.2.1-cp38-abi3-manylinux1_x86_64.whl", "yanked": "broken build"},
    {"filename": "vllm-0.2.1.tar.gz", "yanked": true},
    {"filename": "vllm-0.9.2-cp38-abi3-manylinux1_x86_64.whl", "yanked": false},
    {"filename": "vllm-0.19.0-cp38-abi3-manylinux1_x86_64.whl", "yanked": false},
    {"filename": "vllm-0.27.1-cp38-abi3-manylinux1_x86_64.whl", "yanked": false},
    {"filename": "vllm-0.27.1.tar.gz", "yanked": false}
  ]
}`

func TestLatestPyPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/simple/vllm/", r.URL.Path)
		assert.Equal(t, simpleAPIAccept, r.Header.Get("Accept"))
		w.Header().Set("Content-Type", simpleAPIAccept)
		_, _ = w.Write([]byte(simpleIndexJSON))
	}))
	defer srv.Close()
	res := stubResolver(srv.URL)

	got, err := res.Latest(context.Background(), pypiSource())
	require.NoError(t, err)

	// 0.27.1 over 0.9.2 proves the list is ordered by comparator rather than
	// lexically or by position.
	assert.Equal(t, "0.27.1", got.Version)
	assert.Equal(t, "0.27.1", got.Tag)
	assert.Contains(t, got.URL, "/project/vllm/0.27.1/")
}

func TestLatestPyPISkipsFullyYankedVersion(t *testing.T) {
	// Every file of 0.2.1 is yanked, so the release is retracted. 0.9.2 has a
	// live file and stays a candidate.
	const onlyYankedNewest = `{
	  "versions": ["0.9.2", "0.28.0"],
	  "files": [
	    {"filename": "vllm-0.9.2-cp38-abi3-manylinux1_x86_64.whl", "yanked": false},
	    {"filename": "vllm-0.28.0-cp38-abi3-manylinux1_x86_64.whl", "yanked": true},
	    {"filename": "vllm-0.28.0.tar.gz", "yanked": "bad wheel"}
	  ]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(onlyYankedNewest))
	}))
	defer srv.Close()
	res := stubResolver(srv.URL)

	got, err := res.Latest(context.Background(), pypiSource())
	require.NoError(t, err)
	assert.Equal(t, "0.9.2", got.Version)
}

func TestLatestPyPIKeepsPartiallyYankedVersion(t *testing.T) {
	// One bad wheel does not retract a release.
	const partial = `{
	  "versions": ["0.27.1"],
	  "files": [
	    {"filename": "vllm-0.27.1-cp38-abi3-manylinux1_x86_64.whl", "yanked": true},
	    {"filename": "vllm-0.27.1.tar.gz", "yanked": false}
	  ]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(partial))
	}))
	defer srv.Close()
	res := stubResolver(srv.URL)

	got, err := res.Latest(context.Background(), pypiSource())
	require.NoError(t, err)
	assert.Equal(t, "0.27.1", got.Version)
}

func TestLatestPyPIErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name:    "http error",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			wantErr: "HTTP 404",
		},
		{
			name:    "unreadable body",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
			wantErr: "unreadable",
		},
		{
			name:    "no versions",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"versions":[]}`)) },
			wantErr: "lists no versions",
		},
		{
			name: "nothing orderable",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"versions":["nightly"],"files":[]}`))
			},
			wantErr: "no orderable released version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			res := stubResolver(srv.URL)

			_, err := res.Latest(context.Background(), pypiSource())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLatestRejectsInvalidSource(t *testing.T) {
	// A bad source must fail before any request is made, so this needs no
	// stub server at all.
	_, err := Latest(context.Background(), &config.VersionSource{Type: "gitlab", Repo: "a/b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown type")
}

func TestVersionFromFilename(t *testing.T) {
	tests := []struct{ filename, want string }{
		{"vllm-0.27.1-cp38-abi3-manylinux1_x86_64.whl", "0.27.1"},
		{"vllm-0.27.1.tar.gz", "0.27.1"},
		{"mlx_lm-0.31.3-py3-none-any.whl", "0.31.3"},
		{"mlx_lm-0.31.3.tar.gz", "0.31.3"},
		// PEP 427 build tag sits after the version, so field 1 still wins.
		{"pkg-1.2.3-1-py3-none-any.whl", "1.2.3"},
		{"unrecognized.txt", ""},
		{"noversion.whl", ""},
	}
	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			assert.Equal(t, tt.want, versionFromFilename(tt.filename))
		})
	}
}

// lineStubResolver points the release-page base and the API base at one
// stub, so a test can serve both the /releases/latest redirect and the
// releases list.
func lineStubResolver(url string) *Resolver {
	r := stubResolver(url)
	r.githubAPIBase = url
	return r
}

// llamaCppReleases mirrors what ggml-org/llama.cpp actually publishes
// since 2026-08: build tags carrying the binaries, all marked
// prerelease, alongside a versioned release that GitHub calls latest and
// that ships no binary bundle.
const llamaCppReleases = `[
  {"tag_name":"b10582","draft":false,"prerelease":true},
  {"tag_name":"b10581","draft":false,"prerelease":true},
  {"tag_name":"v0.2.0","draft":false,"prerelease":false},
  {"tag_name":"b10549","draft":false,"prerelease":false}
]`

// llamaCppHandler serves the redirect GitHub actually sends today
// (latest == v0.2.0) plus the releases list, and counts API calls.
func llamaCppHandler(apiCalls *int, releases string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			*apiCalls++
			_, _ = w.Write([]byte(releases))
			return
		}
		w.Header().Set("Location", "/ggml-org/llama.cpp/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	}
}

// TestLatestGitHubReleaseFallsBackWhenLatestIsUnorderable is the defect.
// GitHub reports v0.2.0 as latest; it is not a build number, so
// llama.cpp compared as unknown on every node and no upgrade was ever
// offered. The answer has to come from the newest tag that CAN be
// ordered, which is a prerelease.
func TestLatestGitHubReleaseFallsBackWhenLatestIsUnorderable(t *testing.T) {
	var apiCalls int
	srv := httptest.NewServer(llamaCppHandler(&apiCalls, llamaCppReleases))
	defer srv.Close()

	got, err := lineStubResolver(srv.URL).Latest(context.Background(), buildNumberSource())
	require.NoError(t, err)

	assert.Equal(t, "b10582", got.Tag)
	assert.Equal(t, "10582", got.Version, "Version is normalized, Tag is raw")
	assert.Contains(t, got.URL, "releases/tag/b10582")
	assert.Equal(t, 1, apiCalls, "the fallback costs exactly one API call")
}

// TestLatestGitHubReleaseKeepsTheCheapPath guards the API budget: when
// GitHub's latest is orderable, the redirect answer stands and no REST
// call is made. The 60/hour unauthenticated limit is why the fallback is
// a fallback.
func TestLatestGitHubReleaseKeepsTheCheapPath(t *testing.T) {
	var apiCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			apiCalls++
		}
		w.Header().Set("Location", "/ggml-org/llama.cpp/releases/tag/b10502")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	got, err := lineStubResolver(srv.URL).Latest(context.Background(), buildNumberSource())
	require.NoError(t, err)
	assert.Equal(t, "b10502", got.Tag)
	assert.Zero(t, apiCalls, "an orderable latest must not spend API budget")
}

// TestLatestGitHubReleaseFallbackOrdersByComparator guards against
// trusting GitHub's ordering, which is by publication date: a build
// published late must not outrank a higher build number.
func TestLatestGitHubReleaseFallbackOrdersByComparator(t *testing.T) {
	var apiCalls int
	srv := httptest.NewServer(llamaCppHandler(&apiCalls, `[
	  {"tag_name":"b10400","draft":false},
	  {"tag_name":"b10582","draft":false},
	  {"tag_name":"b10500","draft":false}
	]`))
	defer srv.Close()

	got, err := lineStubResolver(srv.URL).Latest(context.Background(), buildNumberSource())
	require.NoError(t, err)
	assert.Equal(t, "b10582", got.Tag, "list position must not decide the winner")
}

// TestLatestGitHubReleaseFallbackSkipsDrafts — a draft is visible only to
// maintainers and has no downloadable assets, so offering it as an
// upgrade target resolves to a 404.
func TestLatestGitHubReleaseFallbackSkipsDrafts(t *testing.T) {
	var apiCalls int
	srv := httptest.NewServer(llamaCppHandler(&apiCalls, `[
	  {"tag_name":"b10999","draft":true},
	  {"tag_name":"b10582","draft":false}
	]`))
	defer srv.Close()

	got, err := lineStubResolver(srv.URL).Latest(context.Background(), buildNumberSource())
	require.NoError(t, err)
	assert.Equal(t, "b10582", got.Tag)
}

// TestLatestGitHubReleaseFallbackErrors: when nothing orderable exists,
// report that no release resolved rather than falling back to the
// unorderable tag. Returning it is what produced the uncomparable answer
// this fallback exists to fix.
func TestLatestGitHubReleaseFallbackErrors(t *testing.T) {
	t.Run("nothing orderable", func(t *testing.T) {
		var apiCalls int
		srv := httptest.NewServer(llamaCppHandler(&apiCalls, `[{"tag_name":"v0.2.0","draft":false}]`))
		defer srv.Close()

		_, err := lineStubResolver(srv.URL).Latest(context.Background(), buildNumberSource())
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNoUpstreamRelease)
		// The message has to name the tag that started it, or the
		// operator sees "no release" for a repo full of releases.
		assert.Contains(t, err.Error(), "v0.2.0")
	})

	t.Run("rate limited", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/repos/") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Location", "/ggml-org/llama.cpp/releases/tag/v0.2.0")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()

		_, err := lineStubResolver(srv.URL).Latest(context.Background(), buildNumberSource())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "403")
	})

	t.Run("unparseable body", func(t *testing.T) {
		var apiCalls int
		srv := httptest.NewServer(llamaCppHandler(&apiCalls, `not json`))
		defer srv.Close()

		_, err := lineStubResolver(srv.URL).Latest(context.Background(), buildNumberSource())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not decode")
	})
}
