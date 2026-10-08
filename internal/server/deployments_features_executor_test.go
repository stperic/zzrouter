package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeploymentJobIncludesRuntimeProvisioningFailure(t *testing.T) {
	modelregistry.SetModelsRootDirOverride(t.TempDir())
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	registry, err := modelregistry.NewRegistry(func() *pkgConfig.AppsConfig { return nil })
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	tracker := modelregistry.NewDownloadTrackerWithPersistence("")
	t.Cleanup(tracker.Stop)
	reg := newTestJobsRegistry(t)
	var installs atomic.Int32
	e := &DeploymentsExecutor{getNodename: func() string { return "worker" }, downloads: tracker, registry: registry, jobs: reg, jobKeys: map[string]string{}, ensureFeatures: func(context.Context, string, string, []string, []string, func(string)) error {
		installs.Add(1)
		return errors.New("runtime install failed")
	}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	e.downloadHuggingFace(c, metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{}}, "download-key", "mlx", []string{"vision"})
	assert.Equal(t, 202, rec.Code)
	var result internalDeploymentResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	ev := waitJob(t, reg, result.JobID)
	assert.Equal(t, jobs.PhaseFailed, ev.Phase)
	assert.Contains(t, ev.Err, "runtime install failed")
	assert.Equal(t, int32(1), installs.Load())
	status := tracker.GetDownloadStatus("download-key")
	require.NotNil(t, status)
	assert.Equal(t, "failed", status.Status)
}

func TestInternalDeployKeepsConcurrentFeatureObligations(t *testing.T) {
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	registry, err := modelregistry.NewRegistry(func() *pkgConfig.AppsConfig { return nil })
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	tracker := modelregistry.NewDownloadTrackerWithPersistence("")
	t.Cleanup(tracker.Stop)
	reg := newTestJobsRegistry(t)
	var installs atomic.Int32
	e := &DeploymentsExecutor{
		getNodename:           func() string { return "worker" },
		appsConfig:            func() *pkgConfig.AppsConfig { return &pkgConfig.AppsConfig{} },
		isCloudProviderFn:     func(string) bool { return false },
		isLocalNodeCompatible: func(string, string, string) bool { return true },
		downloads:             tracker,
		registry:              registry,
		jobs:                  reg,
		jobKeys:               map[string]string{},
		ensureFeatures: func(_ context.Context, _ string, _ string, names, _ []string, _ func(string)) error {
			if len(names) > 0 {
				assert.Equal(t, []string{"vision"}, names)
				installs.Add(1)
			}
			return nil
		},
	}
	dir := filepath.Join(root, "org/model")
	require.NoError(t, os.MkdirAll(dir, 0700))
	for _, name := range []string{"model.gguf", "mmproj.gguf"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("held"), 0600))
		require.NoError(t, integrity.RecordFiles(dir, "org/model", "", integrity.FileChecksum{RelativePath: name, Size: 4, SHA256: "held"}))
	}
	release, err := integrity.AcquireMaterialization(t.Context(), dir)
	require.NoError(t, err)
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(release) }
	defer unlock()
	var results []internalDeploymentResult
	for _, features := range [][]string{{}, {"vision"}} {
		files := []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: "model.gguf", Size: 4}}}
		if len(features) > 0 {
			files = append(files, metadata.DownloadFile{TreeFileEntry: metadata.TreeFileEntry{Name: "mmproj.gguf", Size: 4}, Feature: "vision"})
		}
		body, err := json.Marshal(map[string]any{"model": "org/model", "registry": "huggingface", "provider": "llamacpp", "features": features, "download": metadata.DownloadRequest{Repo: "org/model", Files: files}})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/deployments", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		e.HandleInternalDeploy(c)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		var result internalDeploymentResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		results = append(results, result)
	}
	assert.NotEqual(t, results[0].Key, results[1].Key)
	assert.NotEqual(t, results[0].JobID, results[1].JobID)
	unlock()
	for _, result := range results {
		assert.Equal(t, jobs.PhaseDone, waitJob(t, reg, result.JobID).Phase)
	}
	assert.EqualValues(t, 1, installs.Load(), "vision job retains its independent provisioning obligation")
	manifest, err := integrity.ReadManifest(dir)
	require.NoError(t, err)
	require.NotNil(t, manifest)
	found := false
	for _, file := range manifest.Files {
		if file.RelativePath == "mmproj.gguf" {
			found = true
			assert.Equal(t, "vision", file.Feature)
		}
	}
	require.True(t, found, "projector must be recorded")
}
