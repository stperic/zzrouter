package modelregistry

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stretchr/testify/require"
)

func TestDownloadTracker_OnCompleteFiresOncePerTransition(t *testing.T) {
	dt := NewDownloadTrackerWithPersistence(filepath.Join(t.TempDir(), "downloads.json"))
	t.Cleanup(dt.Stop)
	var fires atomic.Int32
	dt.SetOnCompleteCallback(func() error { fires.Add(1); return nil })
	const key = "host/huggingface/org/model"
	gen := dt.StartDownload(key, "org/model")
	require.NoError(t, dt.UpdateDownloadFullGen(key, gen, string(constants.StatusCompleted), "", 0, 0, 100, 0, 0, 0))
	require.NoError(t, dt.UpdateDownloadFullGen(key, gen, string(constants.StatusCompleted), "", 0, 0, 100, 0, 0, 0))
	require.EqualValues(t, 1, fires.Load())
}

func TestDownloadTracker_CompletionWaitsForCatalogPublication(t *testing.T) {
	dt := NewDownloadTrackerWithPersistence(filepath.Join(t.TempDir(), "downloads.json"))
	t.Cleanup(dt.Stop)
	entered, release := make(chan struct{}), make(chan struct{})
	returned := make(chan error, 1)
	dt.SetOnCompleteCallback(func() error {
		close(entered)
		<-release
		return nil
	})
	const key = "host/huggingface/org/model"
	gen := dt.StartDownload(key, "org/model")
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
	dt.SetOnCompleteCallback(func() error { return failure })
	gen := dt.StartDownload("key", "org/model")
	require.ErrorIs(t, dt.UpdateDownloadFullGen("key", gen, string(constants.StatusCompleted), "", 1, 1, 100, 0, 0, 0), failure)
	require.Equal(t, string(constants.StatusPending), dt.GetDownloadStatus("key").Status)
}

func TestDownloadTracker_CompletionRechecksGenerationAndCancellation(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "replace"}[replace], func(t *testing.T) {
			dt := NewDownloadTrackerWithPersistence("")
			t.Cleanup(dt.Stop)
			gen := dt.StartDownload("key", "org/model")
			dt.SetOnCompleteCallback(func() error {
				if replace {
					dt.StartDownload("key", "org/new-model")
				} else {
					dt.UpdateDownloadFullGen("key", gen, string(constants.StatusCancelled), "", 0, 0, 0, 0, 0, 0)
				}
				return nil
			})
			require.NoError(t, dt.UpdateDownloadFullGen("key", gen, string(constants.StatusCompleted), "", 1, 1, 100, 0, 0, 0))
			require.NotEqual(t, string(constants.StatusCompleted), dt.GetDownloadStatus("key").Status)
		})
	}
}
