package httperr

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gzipBytes is a test helper that wraps a body in gzip, mirroring
// what an upstream that ignores Accept-Encoding:identity might emit.
func gzipBytes(t *testing.T, raw string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write([]byte(raw))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func TestReadErrorBody(t *testing.T) {
	t.Run("plain body — read verbatim", func(t *testing.T) {
		resp := &http.Response{
			Header: http.Header{},
			Body:   io.NopCloser(strings.NewReader(`{"error":"x"}`)),
		}
		assert.Equal(t, `{"error":"x"}`, string(readUpstreamErrorBody(resp)))
	})

	t.Run("gzip body — gunzipped", func(t *testing.T) {
		gzipped := gzipBytes(t, `{"error":"compressed"}`)
		resp := &http.Response{
			Header: http.Header{"Content-Encoding": []string{"gzip"}},
			Body:   io.NopCloser(bytes.NewReader(gzipped)),
		}
		assert.Equal(t, `{"error":"compressed"}`, string(readUpstreamErrorBody(resp)))
	})

	t.Run("gzip header but plain body — returns nil", func(t *testing.T) {
		resp := &http.Response{
			Header: http.Header{"Content-Encoding": []string{"gzip"}},
			Body:   io.NopCloser(strings.NewReader("not actually gzipped")),
		}
		assert.Nil(t, readUpstreamErrorBody(resp),
			"malformed gzip must surface as nil so downstream synthesizes a status placeholder")
	})

	t.Run("body at exactly the cap — accepted", func(t *testing.T) {
		// Boundary: bodies exactly equal to the cap are valid (well-
		// behaved upstream that happens to use every byte).
		atCap := strings.Repeat("x", maxUpstreamErrorBodyBytes)
		resp := &http.Response{
			Header: http.Header{},
			Body:   io.NopCloser(strings.NewReader(atCap)),
		}
		got := readUpstreamErrorBody(resp)
		assert.Equal(t, maxUpstreamErrorBodyBytes, len(got),
			"body of exactly maxUpstreamErrorBodyBytes must be returned in full")
	})

	t.Run("body exceeds cap — fail-closed nil", func(t *testing.T) {
		// Body strictly larger than maxUpstreamErrorBodyBytes. Returning a
		// truncated mid-token slice would feed garbage into the
		// rewriter and surface a 64-KiB user-facing error string;
		// nil lets downstream synthesize from status instead.
		huge := strings.Repeat("x", maxUpstreamErrorBodyBytes+8192)
		resp := &http.Response{
			Header: http.Header{},
			Body:   io.NopCloser(strings.NewReader(huge)),
		}
		assert.Nil(t, readUpstreamErrorBody(resp),
			"body strictly larger than the cap must fail-closed to nil")
	})

	t.Run("brotli encoding — fail-closed nil", func(t *testing.T) {
		// Cloudflare-fronted backends, OpenAI-via-proxy, and any
		// reverse proxy with brotli enabled by default. Pre-fix the
		// helper passed brotli bytes through to the rewriter,
		// re-introducing F5.
		resp := &http.Response{
			Header: http.Header{"Content-Encoding": []string{"br"}},
			Body:   io.NopCloser(strings.NewReader("compressed-bytes")),
		}
		assert.Nil(t, readUpstreamErrorBody(resp),
			"non-gzip non-identity encoding must fail-closed")
	})

	t.Run("deflate encoding — fail-closed nil", func(t *testing.T) {
		resp := &http.Response{
			Header: http.Header{"Content-Encoding": []string{"deflate"}},
			Body:   io.NopCloser(strings.NewReader("compressed-bytes")),
		}
		assert.Nil(t, readUpstreamErrorBody(resp))
	})

	t.Run("identity encoding — read verbatim", func(t *testing.T) {
		// Some upstreams explicitly set identity rather than omit
		// the header. Treat as plain.
		resp := &http.Response{
			Header: http.Header{"Content-Encoding": []string{"identity"}},
			Body:   io.NopCloser(strings.NewReader(`{"error":"x"}`)),
		}
		assert.Equal(t, `{"error":"x"}`, string(readUpstreamErrorBody(resp)))
	})

	t.Run("Content-Encoding case-insensitive", func(t *testing.T) {
		gzipped := gzipBytes(t, `{"error":"x"}`)
		resp := &http.Response{
			Header: http.Header{"Content-Encoding": []string{"GZIP"}},
			Body:   io.NopCloser(bytes.NewReader(gzipped)),
		}
		assert.Equal(t, `{"error":"x"}`, string(readUpstreamErrorBody(resp)),
			"upstream may emit GZIP/Gzip — strings.EqualFold must match")
	})

}

func TestReadErrorBodyRejectsCorruptGzipTrailer(t *testing.T) {
	raw := gzipBytes(t, `{"error":"images are not supported"}`)
	raw[len(raw)-1] ^= 0xff
	resp := &http.Response{Header: http.Header{"Content-Encoding": []string{"gzip"}}, Body: io.NopCloser(bytes.NewReader(raw))}
	assert.Nil(t, readUpstreamErrorBody(resp), "a readable prefix with a corrupt checksum must not change the error class")
}
