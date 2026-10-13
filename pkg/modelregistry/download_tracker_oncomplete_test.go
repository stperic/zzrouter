package modelregistry

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

func TestDownloadTracker_OnCompleteFiresOncePerTransition(t *testing.T) {
	dt := NewDownloadTrackerWithPersistence(filepath.Join(t.TempDir(), "downloads.json"))
	t.Cleanup(dt.Stop)
	var fires atomic.Int32
	callback := func() error { fires.Add(1); return nil }
	const key = "host/huggingface/org/model"
	gen := dt.StartDownload(key, "org/model", nil, DownloadOptions{OnComplete: callback})
	require.NoError(t, dt.UpdateDownloadFullGen(key, gen, string(constants.StatusCompleted), "", 0, 0, 100, 0, 0, 0))
	require.ErrorIs(t, dt.UpdateDownloadFullGen(key, gen, string(constants.StatusCompleted), "", 0, 0, 100, 0, 0, 0), ErrDownloadInactive)
	require.EqualValues(t, 1, fires.Load())
}

func TestDownloadTrackerCompletionBelongsToAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "downloads.json")
	dt := NewDownloadTrackerWithPersistence(path)
	var fires atomic.Int32
	target := metadata.DownloadTarget{Owner: metadata.OwnerProviderStore, Provider: "studio", Category: "vae", Name: "model.gguf"}
	catalog := dt.StartDownload("catalog", "org/model", nil, DownloadOptions{OnComplete: func() error {
		fires.Add(1)
		return nil
	}})
	provider := dt.StartDownload("provider", "org/model", nil, DownloadOptions{Target: target, RequestedFile: "model.gguf", SourceKey: "source"})
	require.NoError(t, dt.UpdateDownloadFullGen("provider", provider, "completed", "model.gguf", 1, 1, 100, 1, 1, 0))
	require.Zero(t, fires.Load())
	require.Empty(t, dt.GetDownloadStatus("provider").ModelName)
	require.NoError(t, dt.UpdateDownloadFullGen("catalog", catalog, "completed", "", 1, 1, 100, 1, 1, 0))
	require.EqualValues(t, 1, fires.Load())
	dt.Stop()
	restored := NewDownloadTrackerWithPersistence(path)
	t.Cleanup(restored.Stop)
	require.Equal(t, target, restored.GetDownloadStatus("provider").Target)
	require.Equal(t, "source", restored.GetDownloadStatus("provider").SourceKey)
}

func TestDownloadTracker_CompletionWaitsForCatalogPublication(t *testing.T) {
	dt := NewDownloadTrackerWithPersistence(filepath.Join(t.TempDir(), "downloads.json"))
	t.Cleanup(dt.Stop)
	entered, release := make(chan struct{}), make(chan struct{})
	returned := make(chan error, 1)
	callback := func() error {
		close(entered)
		<-release
		return nil
	}
	const key = "host/huggingface/org/model"
	gen := dt.StartDownload(key, "org/model", nil, DownloadOptions{OnComplete: callback})
	go func() {
		returned <- dt.UpdateDownloadFullGen(key, gen, string(constants.StatusCompleted), "", 1, 1, 100, 0, 0, 0)
	}()
	<-entered
	status := dt.GetDownloadStatus(key).Status
	close(release)
	require.NoError(t, <-returned)
	require.NotEqual(t, string(constants.StatusCompleted), status, "completion must wait for catalog publication")
	require.Equal(t, string(constants.StatusCompleted), dt.GetDownloadStatus(key).Status)
}

func TestDownloadTracker_CompletionPublicationFailure(t *testing.T) {
	dt := NewDownloadTrackerWithPersistence("")
	t.Cleanup(dt.Stop)
	failure := errors.New("catalog unavailable")
	callback := func() error { return failure }
	gen := dt.StartDownload("key", "org/model", nil, DownloadOptions{OnComplete: callback})
	require.ErrorIs(t, dt.UpdateDownloadFullGen("key", gen, string(constants.StatusCompleted), "", 1, 1, 100, 0, 0, 0), failure)
	require.Equal(t, string(constants.StatusPending), dt.GetDownloadStatus("key").Status)
}

func TestDownloadTracker_CompletionRechecksGenerationAndCancellation(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "replace"}[replace], func(t *testing.T) {
			dt := NewDownloadTrackerWithPersistence("")
			t.Cleanup(dt.Stop)
			var gen uint64
			callback := func() error {
				if replace {
					dt.StartDownload("key", "org/new-model", nil, DownloadOptions{})
				} else {
					require.NoError(t, dt.UpdateDownloadFullGen("key", gen, string(constants.StatusCancelled), "", 0, 0, 0, 0, 0, 0))
				}
				return nil
			}
			gen = dt.StartDownload("key", "org/model", nil, DownloadOptions{OnComplete: callback})
			require.ErrorIs(t, dt.UpdateDownloadFullGen("key", gen, string(constants.StatusCompleted), "", 1, 1, 100, 0, 0, 0), ErrDownloadInactive)
			require.NotEqual(t, string(constants.StatusCompleted), dt.GetDownloadStatus("key").Status)
		})
	}
}
