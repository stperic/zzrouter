package download

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallRedirectsValidateBeforeTransport(t *testing.T) {
	for _, target := range []string{"https://codeload.github.com/Comfy-Org/ComfyUI/tar.gz/v0.40.0", "https://objects.githubusercontent.com/release", "https://evil.test/archive", "http://codeload.github.com/archive", "https://github.com.evil.test/archive"} {
		t.Run(target, func(t *testing.T) {
			calls := []string{}
			client := NewClient(time.Second)
			client.Transport = resumeTransport(func(req *http.Request) (*http.Response, error) {
				calls = append(calls, req.URL.String())
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: req}
				if len(calls) == 1 {
					response.StatusCode = http.StatusFound
					response.Header.Set("Location", target)
				}
				return response, nil
			})
			response, err := client.Get("https://github.com/Comfy-Org/ComfyUI/archive.tar.gz")
			if strings.HasPrefix(target, "https://codeload.github.com/") || strings.HasPrefix(target, "https://objects.githubusercontent.com/") {
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				assert.Equal(t, []string{"https://github.com/Comfy-Org/ComfyUI/archive.tar.gz", target}, calls)
			} else {
				require.Error(t, err)
				require.Len(t, calls, 1, "forbidden redirects must never reach transport")
			}
		})
	}
	client, calls := NewClient(time.Second), 0
	client.Transport = resumeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{fmt.Sprintf("https://github.com/loop/%d", calls)}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	response, err := client.Get("https://github.com/loop/0")
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorContains(t, err, "redirect limit")
	assert.Equal(t, 10, calls)
}

func TestSourceFetchBoundAndNetworkFailureLeaveNoFiles(t *testing.T) {
	for _, declaredSize := range []int64{-1, 100} {
		t.Run(fmt.Sprint(declaredSize), func(t *testing.T) {
			transport := resumeTransport(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: declaredSize, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 100))), Request: req}, nil
			})
			original := fetchTransport
			fetchTransport = transport
			t.Cleanup(func() { fetchTransport = original })
			dir := t.TempDir()
			_, err := FetchForProvider(t.Context(), "comfyui", "https://github.com/Comfy-Org/ComfyUI/archive.tar.gz", dir, nil, Options{MaxSize: 20})
			require.Error(t, err)
			files, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
	calls := 0
	transport := resumeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 10, Header: http.Header{}, Body: io.NopCloser(interruptedBody{strings.NewReader("partial")}), Request: req}, nil
	})
	original := fetchTransport
	fetchTransport = transport
	t.Cleanup(func() { fetchTransport = original })
	dir := t.TempDir()
	_, err := FetchForProvider(t.Context(), "comfyui", "https://github.com/Comfy-Org/ComfyUI/archive.tar.gz", dir, nil, Options{MaxSize: 20})
	require.Error(t, err)
	assert.Greater(t, calls, 1, "existing download retries remain in effect")
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestSourceFetchFollowsApprovedRedirectAndKeepsPinnedArtifact(t *testing.T) {
	var archive bytes.Buffer
	zipped := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zipped)
	for name, body := range map[string]string{"release/requirements.txt": "torch>=2\n", "release/main.py": "print('help')\n"} {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Typeflag: tar.TypeReg, Size: int64(len(body))}))
		_, err := writer.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	require.NoError(t, zipped.Close())
	source := config.InstallSource{SHA256: fmt.Sprintf("%x", sha256.Sum256(archive.Bytes())), Requirements: "requirements.txt", URL: "https://github.com/Comfy-Org/ComfyUI/archive.tar.gz"}
	calls := 0
	original := fetchTransport
	fetchTransport = resumeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host == "github.com" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://codeload.github.com/Comfy-Org/ComfyUI/archive.tar.gz"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		require.Equal(t, "codeload.github.com", req.URL.Host)
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(archive.Len()), Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(archive.Bytes())), Request: req}, nil
	})
	t.Cleanup(func() { fetchTransport = original })
	candidate := t.TempDir()
	cache := filepath.Join(candidate, source.SHA256)
	result, err := FetchForProvider(t.Context(), "comfyui", source.URL, cache, nil, Options{MaxSize: 64 << 20})
	require.NoError(t, err)
	require.NoError(t, security.VerifySHA256(result.FilePath, source.SHA256))
	body, err := os.ReadFile(result.FilePath)
	require.NoError(t, err)
	require.Equal(t, archive.Bytes(), body)
	require.NoError(t, security.VerifySHA256(result.FilePath, source.SHA256))
	require.Equal(t, 2, calls, "one fetch plus its redirect retains the complete artifact")
	require.NoError(t, os.WriteFile(result.FilePath, []byte("tampered"), 0600))
	require.ErrorContains(t, security.VerifySHA256(result.FilePath, source.SHA256), "checksum mismatch")
	require.Equal(t, 2, calls)
}
