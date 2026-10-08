package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	modelsync "github.com/stperic/zzrouter/pkg/model/sync"
	"github.com/stperic/zzrouter/pkg/modelregistry"
)

// A deploy asks every node whether it holds the plan; the answer comes from
// the node's manifest, so it holds for weights it could not even read, and
// a node missing the projector does not hold a vision plan.
func TestSyncExistsAnswersFromTheManifest(t *testing.T) {
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	dir := filepath.Join(root, "org", "model")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	weights := filepath.Join(dir, "model.gguf")
	require.NoError(t, os.WriteFile(weights, []byte("weights"), 0o600))
	require.NoError(t, integrity.RecordFiles(dir, "org/model", "", integrity.FileChecksum{RelativePath: "model.gguf", Size: 7}))
	require.NoError(t, os.Chmod(weights, 0))
	t.Cleanup(func() { _ = os.Chmod(weights, 0o600) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/exists", (&SyncExecutor{security: modelsync.NewSecurity()}).HandleCheckModelExists)
	exists := func(body string) bool {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/exists?model=org/model&format=gguf", strings.NewReader(body)))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got struct{ Exists bool }
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		return got.Exists
	}
	assert.True(t, exists(`{"Repo":"org/model","Files":[{"path":"model.gguf","size":7}]}`))
	assert.False(t, exists(`{"Repo":"org/model","Files":[{"path":"model.gguf","size":7},{"path":"mmproj-F16.gguf","size":9,"feature":"vision"}]}`))
}
