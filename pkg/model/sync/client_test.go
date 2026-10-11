package sync

import (
	"crypto/sha256"
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

func TestDownloadFileResumesWithClusterClient(t *testing.T) {

	const content = "abcdefghijklmnopqrstuvwxyz"
	calls := 0
	transport := resumeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, "/zzrouter/v1/internal/sync/file", req.URL.Path)
		assert.Equal(t, "org/model", req.URL.Query().Get("model"))
		assert.Equal(t, "model.gguf", req.URL.Query().Get("file"))
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
	digest := sha256.Sum256([]byte(content))
	sum := fmt.Sprintf("%x", digest)

	c := NewClient(ClientConfig{HTTPClient: &http.Client{Transport: transport}})
	got, err := c.downloadFile(t.Context(), "https://coordinator.test", "org/model", "gguf", FileChecksum{Name: "model.gguf", Size: int64(len(content)), SHA256: sum}, dest, nil)
	assert.EqualValues(t, len(content), got)

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
