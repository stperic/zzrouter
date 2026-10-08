package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectedSyncPreservesRolesAndSkipsWeights(t *testing.T) {
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	source := t.TempDir()
	bodies := map[string]string{"model.gguf": "weights", "mmproj.gguf": "projector", "other.gguf": "unselected"}
	req := metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{}}
	for _, name := range []string{"model.gguf", "mmproj.gguf"} {
		body := bodies[name]
		sum := sha256.Sum256([]byte(body))
		role := ""
		if name == "mmproj.gguf" {
			role = "vision"
		}
		req.Files = append(req.Files, metadata.DownloadFile{TreeFileEntry: metadata.TreeFileEntry{Name: name, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, Feature: role})
	}
	for name, body := range bodies {
		p := filepath.Join(source, req.Repo, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0600))
	}
	dir := filepath.Join(root, req.Repo)
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model.gguf"), []byte("weights"), 0600))
	var fetched atomic.Int32
	security := NewSecurityFromRoot(source)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zzrouter/v1/internal/sync/manifest" {
			var selection metadata.DownloadRequest
			if err := json.NewDecoder(r.Body).Decode(&selection); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			manifest, err := security.GenerateManifest(selection, "gguf")
			if err != nil {
				http.Error(w, err.Error(), 404)
				return
			}
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		fetched.Add(1)
		_, _ = w.Write([]byte(bodies[r.URL.Query().Get("file")]))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{HTTPClient: srv.Client()})
	result, err := c.SyncModel(context.Background(), srv.URL, req, "gguf", nil)
	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, 1, result.FilesDownloaded)
	assert.Equal(t, 1, result.FilesSkipped)
	assert.Equal(t, int32(1), fetched.Load())
	manifest, err := integrity.ReadManifest(dir)
	require.NoError(t, err)
	require.NotNil(t, manifest)
	roles := map[string]string{}
	for _, f := range manifest.Files {
		roles[f.RelativePath] = f.Feature
	}
	assert.Equal(t, map[string]string{"model.gguf": "", "mmproj.gguf": "vision"}, roles)
	_, err = os.Stat(filepath.Join(dir, "other.gguf"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, "gguf"))
	assert.True(t, os.IsNotExist(err), "sync uses the downloader's repository layout")
	req.Force = true
	result, err = c.SyncModel(context.Background(), srv.URL, req, "gguf", nil)
	require.NoError(t, err)
	assert.Equal(t, 2, result.FilesDownloaded)
	assert.Equal(t, int32(3), fetched.Load())
}

func TestManifestRequiresCompleteSelectedSet(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "org", "model")
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "weights.gguf"), []byte("weights"), 0600))
	req := metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: "weights.gguf", Size: 7}}, {TreeFileEntry: metadata.TreeFileEntry{Name: "mmproj.gguf", Size: 9}, Feature: "vision"}}}
	_, err := NewSecurityFromRoot(root).GenerateManifest(req, "gguf")
	assert.ErrorContains(t, err, "selected files are missing")
}

func TestWritePathRejectsSymlinkEscapeBeforeCreating(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "org")))
	_, err := NewSecurityFromRoot(root).ValidateWritePath("org/model/projector.gguf")
	assert.ErrorContains(t, err, "escapes")
	_, err = os.Stat(filepath.Join(outside, "model"))
	assert.True(t, os.IsNotExist(err))
}

// Whether a node holds a plan is read from its manifest, never by hashing:
// an unreadable weights file recorded at the planned size and hash is held,
// and a file missing, at another size, or with another hash is not.
func TestHoldsReadsTheManifestNotTheBytes(t *testing.T) {
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	dir := filepath.Join(root, "org", "model")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	weights := filepath.Join(dir, "model.gguf")
	require.NoError(t, os.WriteFile(weights, []byte("weights"), 0o600))
	require.NoError(t, integrity.RecordFiles(dir, "org/model", "", integrity.FileChecksum{RelativePath: "model.gguf", Size: 7, SHA256: "abc"}))
	require.NoError(t, os.Chmod(weights, 0))
	t.Cleanup(func() { _ = os.Chmod(weights, 0o600) })

	plan := func(files ...metadata.TreeFileEntry) metadata.DownloadRequest {
		req := metadata.DownloadRequest{Repo: "org/model"}
		for _, f := range files {
			req.Files = append(req.Files, metadata.DownloadFile{TreeFileEntry: f})
		}
		return req
	}
	s := NewSecurity()
	for _, tc := range []struct {
		name string
		req  metadata.DownloadRequest
		want bool
	}{
		{"held", plan(metadata.TreeFileEntry{Name: "model.gguf", Size: 7, SHA256: "ABC"}), true},
		{"other size", plan(metadata.TreeFileEntry{Name: "model.gguf", Size: 8}), false},
		{"other hash", plan(metadata.TreeFileEntry{Name: "model.gguf", Size: 7, SHA256: "def"}), false},
		{"missing file", plan(metadata.TreeFileEntry{Name: "model.gguf", Size: 7}, metadata.TreeFileEntry{Name: "mmproj.gguf", Size: 9}), false},
	} {
		got, err := s.Holds(tc.req)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
}
