package huggingface

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// A distinct concrete transport exercises replacement without atomic.Value's
// same-concrete-type restriction on stored interfaces.
type statusTransport struct{ status int }

func (s statusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp := targetResponse(r, `{}`)
	resp.StatusCode = s.status
	return resp, nil
}

func TestConnectorTransportIsPerInstanceAndCoversEveryHFRequest(t *testing.T) {
	hfc := &Connector{modelsDir: t.TempDir(), token: "fixture-token", apiBase: huggingFaceAPIBase, downloadBase: huggingFaceDownloadBase}
	other := &Connector{apiBase: huggingFaceAPIBase}
	other.SetHTTPTransport(statusTransport{http.StatusNotFound})
	var paths []string
	hfc.SetHTTPTransport(targetTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		paths = append(paths, r.URL.RequestURI())
		if r.URL.RawQuery == "blobs=true" {
			return targetResponse(r, `{"siblings":[{"rfilename":"model.safetensors","size":7}]}`), nil
		}
		if r.URL.Path == "/org/repo/resolve/main/model.safetensors" {
			return targetResponse(r, "weights"), nil
		}
		return targetResponse(r, `{"pipeline_tag":"text-to-image"}`), nil
	}))

	exists, err := hfc.ModelExists(t.Context(), "org/repo")
	require.NoError(t, err)
	require.True(t, exists)
	exists, err = other.ModelExists(t.Context(), "org/repo")
	require.NoError(t, err)
	require.False(t, exists, "another connector retains its own transport")
	info, err := hfc.ShowModel(t.Context(), "org/repo")
	require.NoError(t, err)
	require.Equal(t, "text-to-image", info["details"].(map[string]any)["task"])
	req, err := ParseSource("org/repo#model.safetensors")
	require.NoError(t, err)
	dest := targetDestination{dir: t.TempDir(), name: "local.safetensors"}
	require.NoError(t, hfc.Download(t.Context(), req, nil, dest))
	data, err := os.ReadFile(filepath.Join(dest.dir, dest.name))
	require.NoError(t, err)
	require.Equal(t, "weights", string(data))
	require.Equal(t, []string{"/api/models/org/repo", "/api/models/org/repo", "/api/models/org/repo?blobs=true", "/org/repo/resolve/main/model.safetensors"}, paths)
	hfc.SetHTTPTransport(nil)
	require.Nil(t, hfc.httpClient(httpTimeout).Transport, "nil restores net/http's default behavior without changing the global")
	require.Nil(t, (&Connector{}).httpClient(httpTimeout).Transport)
}

func TestConnectorTransportCanChangeDuringRequests(t *testing.T) {
	hfc := &Connector{apiBase: huggingFaceAPIBase}
	first := statusTransport{http.StatusOK}
	second := targetTransport(func(r *http.Request) (*http.Response, error) { return targetResponse(r, `{}`), nil })
	hfc.SetHTTPTransport(first)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1000 {
			hfc.SetHTTPTransport(first)
			hfc.SetHTTPTransport(second)
		}
	})
	errs := make(chan error, 1)
	wg.Go(func() {
		for range 1000 {
			if _, err := hfc.ModelExists(context.Background(), "org/repo"); err != nil {
				errs <- err
				return
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
