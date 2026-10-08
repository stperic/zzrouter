package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/apipath"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
)

// assetTestNode is a coordinator with the shipped providers, whose
// llamacpp schema declares chat-template-file an asset, plus the
// operator asset t.jinja.
func assetTestNode(t *testing.T) (*Server, assets.Dir) {
	t.Helper()
	cfg := DefaultTestNodeConfig()
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)
	_, err := server.configStore.WriteAsset("llamacpp", "t.jinja", []byte("{{ messages }}"))
	require.NoError(t, err)
	dir, err := server.configStore.Assets("llamacpp")
	require.NoError(t, err)
	return server, dir
}

func assetRequest(t *testing.T, s *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("X-API-Key", TestAdminKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	return w
}

func TestProviderAssetsAPI_Lifecycle(t *testing.T) {
	s, dir := assetTestNode(t)
	path := apipath.ProviderAsset("llamacpp", "mine.jinja")

	w := assetRequest(t, s, http.MethodPut, path, []byte("v1"))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body)
	var info assetDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &info))
	assert.Equal(t, assetDTO{Name: "mine.jinja", Size: 2, SHA256: assets.Digest([]byte("v1")), ReferencedBy: []string{}}, info)

	w = assetRequest(t, s, http.MethodPut, path, []byte("v2"))
	assert.Equal(t, http.StatusOK, w.Code, "a second PUT replaces")

	w = assetRequest(t, s, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, "v2", w.Body.String())

	require.NoError(t, s.configStore.SetAppParameter("llamacpp", "chat-template-file", "mine.jinja"))
	w = assetRequest(t, s, http.MethodGet, apipath.ProviderAssets("llamacpp"), nil)
	require.Equal(t, http.StatusOK, w.Code)
	var list assetListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	byName := map[string]assetDTO{}
	for _, a := range list.Assets {
		byName[a.Name] = a
	}
	require.Len(t, byName, 2, "mine.jinja and t.jinja")
	assert.Equal(t, []string{"defaults.parameters.chat-template-file"}, byName["mine.jinja"].ReferencedBy)
	assert.Empty(t, byName["t.jinja"].ReferencedBy)
	assert.False(t, byName["mine.jinja"].Shipped)

	w = assetRequest(t, s, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusConflict, w.Code, "a named asset is not removed")
	assert.Contains(t, w.Body.String(), `"code":"asset_in_use"`)
	assert.Contains(t, w.Body.String(), `"key":"defaults.parameters.chat-template-file"`)

	require.NoError(t, s.configStore.SetAppParameter("llamacpp", "chat-template-file", "t.jinja"))
	w = assetRequest(t, s, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
	_, err := dir.Path("mine.jinja")
	assert.ErrorIs(t, err, assets.ErrNotFound)
	assert.Equal(t, http.StatusNotFound, assetRequest(t, s, http.MethodGet, path, nil).Code)
}

func TestProviderAssetsAPI_Refusals(t *testing.T) {
	s, _ := assetTestNode(t)

	w := assetRequest(t, s, http.MethodPut, apipath.ProviderAsset("llamacpp", ".hidden"), []byte("x"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"invalid_value"`)

	w = assetRequest(t, s, http.MethodPut, apipath.ProviderAsset("llamacpp", "big.bin"), make([]byte, assets.MaxAssetBytes+1))
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))

	w = assetRequest(t, s, http.MethodDelete, apipath.ProviderAsset("llamacpp", "absent.jinja"), nil)
	assert.Equal(t, http.StatusNotFound, w.Code, "a missing asset is 404 before any reference check")

	w = assetRequest(t, s, http.MethodPut, apipath.ProviderAsset("no-such-provider", "x.jinja"), []byte("x"))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, http.StatusNotFound, assetRequest(t, s, http.MethodGet, apipath.ProviderAssets("no-such-provider"), nil).Code)
}

// /resolved reports the content digest of the asset an asset-typed value
// names, and its ETag moves when that content does.
func TestResolved_ReportsAssetDigest(t *testing.T) {
	s, _ := assetTestNode(t)
	require.NoError(t, s.configStore.SetAppParameter("llamacpp", "chat-template-file", "t.jinja"))

	resolved := func() (resolvedResponse, string) {
		w := assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/llamacpp/resolved", nil)
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
		var r resolvedResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
		return r, w.Header().Get("ETag")
	}
	r, etag := resolved()
	assert.Equal(t, assets.Digest([]byte("{{ messages }}")), r.Parameters["chat-template-file"].SHA256)

	_, err := s.configStore.WriteAsset("llamacpp", "t.jinja", []byte("changed"))
	require.NoError(t, err)
	r, etag2 := resolved()
	assert.Equal(t, assets.Digest([]byte("changed")), r.Parameters["chat-template-file"].SHA256)
	assert.NotEqual(t, etag, etag2, "the same config naming different bytes is a different view")
}

// A file placed by hand that breaks the caps is the stored set's fault,
// not the request's: reads answer 500 with the reason, not 413.
func TestProviderAssetsAPI_OversizedStrayIsNot413(t *testing.T) {
	s, _ := assetTestNode(t)
	require.NoError(t, os.WriteFile(filepath.Join(s.configStore.DirPath(), "on-demand", "llamacpp", "assets", "stray.bin"),
		make([]byte, assets.MaxAssetBytes+1), 0o600))

	w := assetRequest(t, s, http.MethodGet, apipath.ProviderAssets("llamacpp"), nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	w = assetRequest(t, s, http.MethodPut, apipath.ProviderAsset("llamacpp", "small.jinja"), []byte("tiny"))
	assert.Equal(t, http.StatusInternalServerError, w.Code, "a small body is not too large: %s", w.Body)
}

// A worker's assets are a copy the next sync overwrites, so a worker
// mounts no asset routes and a write there is sent to the coordinator.
func TestProviderAssetsAPI_WorkerWritesAreMisdirected(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{
		AdminKey:         TestAdminKey,
		ClusterKey:       TestClusterKey,
		ClusterMode:      pkgConfig.ClusterModeWorker,
		SeedProvidersDir: true,
	})
	path := apipath.ProviderAsset("llamacpp", "mine.jinja")

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		w := assetRequest(t, s, method, path, []byte("x"))
		assert.Equal(t, http.StatusMisdirectedRequest, w.Code, method)
	}
	dir, err := s.configStore.Assets("llamacpp")
	require.NoError(t, err)
	_, err = dir.Path("mine.jinja")
	assert.ErrorIs(t, err, assets.ErrNotFound, "nothing was written")
}

// A name that differs from an existing asset only in case would be the
// same file on a macOS or Windows worker.
func TestProviderAssetsAPI_CaseCollisionConflicts(t *testing.T) {
	s, _ := assetTestNode(t)
	w := assetRequest(t, s, http.MethodPut, apipath.ProviderAsset("llamacpp", "T.jinja"), []byte("x"))
	assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body)
}

// A file placed by hand over the per-asset cap blocks every write to its
// provider; the API can still remove it.
func TestProviderAssetsAPI_DeletesOversizedStray(t *testing.T) {
	s, dir := assetTestNode(t)
	providerDir := filepath.Join(s.configStore.DirPath(), "on-demand", "llamacpp")
	require.NoError(t, os.WriteFile(filepath.Join(providerDir, assets.DirName, "huge.bin"), make([]byte, assets.MaxAssetBytes+1), 0o600))

	w := assetRequest(t, s, http.MethodDelete, apipath.ProviderAsset("llamacpp", "huge.bin"), nil)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body)
	assert.ErrorIs(t, dir.Exists("huge.bin"), assets.ErrNotFound)
}

// A reference at a node or overlay site holds an asset as firmly as one
// in defaults.
func TestProviderAssetsAPI_DeleteSeesEverySite(t *testing.T) {
	s, _ := assetTestNode(t)
	node := s.node.Nodename()
	body := map[string]any{
		"nodes": map[string]any{node: map[string]any{"models": map[string]any{"m": map[string]any{"parameters": map[string]any{"chat-template-file": "t.jinja"}}}}},
		"models": map[string]any{"m": map[string]any{
			"parameters": map[string]any{"ctx-size": 4096},
			"endpoints":  map[string]any{"embeddings": map[string]any{"parameters": map[string]any{"chat-template-file": "t.jinja"}}}}},
	}
	resp := makeAuthRequest(t, s, "PATCH", apipath.ProviderParameters("llamacpp"), TestAdminKey, body)
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)

	w := assetRequest(t, s, http.MethodDelete, apipath.ProviderAsset("llamacpp", "t.jinja"), nil)
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), `"key":"models.m.endpoints.embeddings.parameters.chat-template-file"`)
	assert.Contains(t, w.Body.String(), `"key":"nodes.`+node+`.models.m.parameters.chat-template-file"`)
}

// An asset write reports what it means for the running models, and a
// restart value it does not know is refused before anything is written.
func TestProviderAssetsAPI_ReportsRunsAndChecksRestart(t *testing.T) {
	s, dir := assetTestNode(t)
	path := apipath.ProviderAsset("llamacpp", "new.jinja")

	w := assetRequest(t, s, http.MethodPut, path+"?restart=all", []byte("x"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.ErrorIs(t, dir.Exists("new.jinja"), assets.ErrNotFound, "a refused restart value wrote nothing")

	w = assetRequest(t, s, http.MethodPut, path+"?restart=affected", []byte("x"))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body)
	var got struct {
		Name string      `json:"name"`
		Runs *RunsReport `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "new.jinja", got.Name, "the asset is still the body")
	require.NotNil(t, got.Runs)
	assert.Empty(t, got.Runs.Stale)
}
