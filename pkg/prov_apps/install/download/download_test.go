package download

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchResumesInstallArtifact(t *testing.T) {

	const content = "abcdefghijklmnopqrstuvwxyz"
	calls := 0
	transport := resumeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, "github.com", req.URL.Host)
		resp := &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(content)), Header: make(http.Header)}
		if calls == 1 {
			assert.Empty(t, req.Header.Get("Range"))
			resp.Body = io.NopCloser(interruptedBody{strings.NewReader(content[:3])})
		} else {
			assert.Equal(t, "bytes=3-", req.Header.Get("Range"))
			resp.StatusCode = http.StatusPartialContent
			resp.ContentLength -= 3
			resp.Header.Set("Content-Range", fmt.Sprintf("bytes 3-%d/%d", len(content)-1, len(content)))
			resp.Body = io.NopCloser(strings.NewReader(content[3:]))
		}
		return resp, nil
	})
	dir := t.TempDir()
	dest := filepath.Join(dir, "model.gguf")

	original := fetchTransport
	fetchTransport = transport
	t.Cleanup(func() { fetchTransport = original })
	result, err := Fetch(t.Context(), "https://github.com/org/repo/model.gguf", dir, nil, Options{})
	require.NoError(t, err)
	assert.Equal(t, dest, result.FilePath)
	assert.EqualValues(t, len(content), result.Size)

	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

type resumeTransport func(*http.Request) (*http.Response, error)

func (f resumeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type interruptedBody struct{ io.Reader }

func (b interruptedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
