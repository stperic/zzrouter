package huggingface

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

type targetTransport func(*http.Request) (*http.Response, error)

func (f targetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type targetDestination struct{ dir, name string }

func (d targetDestination) Directory() (string, error) { return d.dir, nil }
func (d targetDestination) CheckSpace(_ int64) error   { return nil }
func (d targetDestination) NoReplace() bool            { return true }
func (d targetDestination) Path(_ string) (string, error) {
	return filepath.Join(d.dir, d.name), nil
}

func providerFileRequest() metadata.DownloadRequest {
	return metadata.DownloadRequest{Repo: "org/model", Revision: "refs/pr/7", Weights: "model-00001-of-00002.gguf",
		Target: metadata.DownloadTarget{Owner: metadata.OwnerProviderStore, Provider: "studio", Category: "vae", Name: "chosen.gguf"}}
}

func targetResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"Content-Length": {strconv.Itoa(len(body))}},
		ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}
}

func TestProviderDownloadExactRevisionAuthAndIntegrity(t *testing.T) {
	const content = "revision-seven-weights"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	listing, err := json.Marshal(map[string]any{"siblings": []map[string]any{
		{"rfilename": "model-00001-of-00002.gguf", "size": len(content), "lfs": map[string]any{"oid": hash}},
		{"rfilename": "model-00002-of-00002.gguf", "size": 100}, {"rfilename": "README.md"},
	}})
	require.NoError(t, err)
	var paths []string
	hfc := &Connector{modelsDir: t.TempDir(), token: "gated-fixture-token", apiBase: huggingFaceAPIBase, downloadBase: huggingFaceDownloadBase}
	hfc.SetHTTPTransport(targetTransport(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.EscapedPath())
		require.Equal(t, "Bearer gated-fixture-token", r.Header.Get("Authorization"))
		if len(paths) == 1 {
			require.Equal(t, "/api/models/org/model/revision/refs%2Fpr%2F7", r.URL.EscapedPath())
			return targetResponse(r, string(listing)), nil
		}
		require.Equal(t, "/org/model/resolve/refs%2Fpr%2F7/model-00001-of-00002.gguf", r.URL.EscapedPath())
		return targetResponse(r, content), nil
	}))
	dest := targetDestination{dir: t.TempDir(), name: "chosen.gguf"}
	req := providerFileRequest()
	require.NoError(t, hfc.Download(t.Context(), req, nil, dest))
	require.Len(t, paths, 2, "fetch only the exact shard, not its siblings")
	bytes, err := os.ReadFile(filepath.Join(dest.dir, dest.name))
	require.NoError(t, err)
	require.Equal(t, content, string(bytes))
	require.Empty(t, onDisk(t, hfc.modelsDir))
	manifest, err := integrity.ReadManifest(dest.dir)
	require.NoError(t, err)
	require.Len(t, manifest.Files, 1)
	require.Equal(t, dest.name, manifest.Files[0].RelativePath)
	require.Equal(t, hash, manifest.Files[0].SHA256)
	// Already landed files never enter the LLM repair/force path.
	req.Files = []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: req.Weights, Size: int64(len(content))}}}
	require.ErrorIs(t, hfc.Download(t.Context(), req, nil, dest), os.ErrExist)
	require.Len(t, paths, 2)
	req.Force = true
	require.Error(t, hfc.Download(t.Context(), req, nil, dest))
}

func TestProviderDownloadRechecksExistingFileUnderMaterializationGate(t *testing.T) {
	dest := targetDestination{dir: t.TempDir(), name: "chosen.gguf"}
	req := providerFileRequest()
	req.Files = []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: req.Weights}}}
	hfc := &Connector{modelsDir: t.TempDir()}
	var fetched atomic.Int32
	hfc.SetHTTPTransport(targetTransport(func(r *http.Request) (*http.Response, error) {
		fetched.Add(1)
		return targetResponse(r, "weights"), nil
	}))
	release, err := integrity.AcquireMaterialization(t.Context(), dest.dir)
	require.NoError(t, err)
	var once sync.Once
	unlock := func() { once.Do(release) }
	defer unlock()
	ctx := &materializationContext{Context: t.Context(), waiting: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- hfc.Download(ctx, req, nil, dest) }()
	// Done is evaluated by the gate's select after all pre-gate work.
	<-ctx.waiting
	err = os.WriteFile(filepath.Join(dest.dir, dest.name), []byte("manual"), 0600)
	unlock()
	require.NoError(t, err)
	require.ErrorIs(t, <-result, os.ErrExist)
	bytes, err := os.ReadFile(filepath.Join(dest.dir, dest.name))
	require.NoError(t, err)
	require.Equal(t, "manual", string(bytes))
	require.Zero(t, fetched.Load(), "the under-gate absence check must refuse before any fetch")
}

type materializationContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *materializationContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestProviderDownloadFailureAndCancellationRemovePartials(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hfc := &Connector{modelsDir: t.TempDir(), downloadBase: huggingFaceDownloadBase}
			hfc.SetHTTPTransport(targetTransport(func(r *http.Request) (*http.Response, error) {
				if cancelled {
					cancel()
					return nil, ctx.Err()
				}
				return targetResponse(r, "wrong hash"), nil
			}))
			req := providerFileRequest()
			req.Files = []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: req.Weights, SHA256: "wrong"}}}
			dest := targetDestination{dir: t.TempDir(), name: "chosen.gguf"}
			err := hfc.Download(ctx, req, nil, dest)
			require.Error(t, err)
			if cancelled {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Empty(t, onDisk(t, dest.dir), "no final, partial or manifest on failure")
		})
	}
}
