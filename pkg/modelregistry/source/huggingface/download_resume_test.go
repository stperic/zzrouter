package huggingface

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownload_CompletesMetadataOnlyDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "google", "gemma-4-E2B-it")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	held := []string{".gitattributes", "README.md", "chat_template.jinja", "config.json", "generation_config.json"}
	for _, name := range held {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600))
	}
	manifest, err := integrity.CreateManifest(t.Context(), dir, "google/gemma-4-E2B-it", "")
	require.NoError(t, err)
	require.NoError(t, integrity.WriteManifest(dir, manifest))
	hfc := &Connector{modelsDir: root, downloadBase: "https://hub.test/"}
	models, err := hfc.ScanModels()
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.True(t, models[0].Incomplete)
	var fetched []string
	original := http.DefaultTransport
	http.DefaultTransport = resumeTransport(func(req *http.Request) (*http.Response, error) {
		fetched = append(fetched, filepath.Base(req.URL.Path))
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 2, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	missing := []string{"model.safetensors", "tokenizer.json", "tokenizer_config.json", "processor_config.json"}
	req := metadata.DownloadRequest{Repo: "google/gemma-4-E2B-it"}
	for _, name := range append(held, missing...) {
		req.Files = append(req.Files, metadata.DownloadFile{TreeFileEntry: metadata.TreeFileEntry{Name: name, Size: 2}})
	}
	require.NoError(t, hfc.Download(t.Context(), req, nil, CatalogDestination(filepath.Join(hfc.modelsDir, filepath.FromSlash(req.Repo)))))
	assert.Equal(t, missing, fetched)
	models, err = hfc.ScanModels()
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.False(t, models[0].Incomplete)
	manifest, err = integrity.ReadManifest(dir)
	require.NoError(t, err)
	require.Len(t, manifest.Files, len(req.Files))
}

func TestFetchResumesWithRegistryAuth(t *testing.T) {

	const content = "abcdefghijklmnopqrstuvwxyz"
	calls := 0
	transport := resumeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, "Bearer secret", req.Header.Get("Authorization"))
		assert.Equal(t, "/org/model/resolve/main/model.gguf", req.URL.Path)
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

	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original })
	hfc := &Connector{downloadBase: "https://hub.test/", token: "secret"}
	got, err := hfc.fetch(t.Context(), "org/model", "main", metadata.TreeFileEntry{Name: "model.gguf", Size: int64(len(content)), SHA256: sum}, dest, nil)
	assert.Equal(t, sum, got)

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
