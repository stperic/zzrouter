package modelregistry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/constants"
)

func TestDeploymentTracker_EvictsFinishedDeployments(t *testing.T) {
	tracker := NewDeploymentTracker()

	stale := tracker.CreateDeployment("old", "gguf", "hf", "", []string{"a"})
	require.NoError(t, tracker.UpdateDeploymentStatus(stale.ID, constants.StatusCompleted, "done"))
	tracker.mu.Lock()
	tracker.deployments[stale.ID].UpdatedAt = time.Now().Add(-terminalRetention - time.Minute)
	tracker.mu.Unlock()

	recent := tracker.CreateDeployment("recent", "gguf", "hf", "", []string{"a"})
	require.NoError(t, tracker.UpdateDeploymentStatus(recent.ID, constants.StatusCompleted, "done"))

	running := tracker.CreateDeployment("running", "gguf", "hf", "", []string{"a"})
	require.NoError(t, tracker.UpdateDeploymentStatus(running.ID, constants.StatusDownloading, ""))

	// The sweep runs on the next create.
	tracker.CreateDeployment("trigger", "gguf", "hf", "", []string{"a"})

	assert.Nil(t, tracker.GetDeployment(stale.ID), "a finished deployment past retention must be evicted")
	assert.NotNil(t, tracker.GetDeployment(recent.ID), "a recent result stays queryable")
	assert.NotNil(t, tracker.GetDeployment(running.ID), "an in-flight deployment is never evicted")
}

func TestDeploymentTracker_NeverEvictsInFlightHowever(t *testing.T) {
	tracker := NewDeploymentTracker()

	slow := tracker.CreateDeployment("slow", "gguf", "hf", "", []string{"a"})
	require.NoError(t, tracker.UpdateDeploymentStatus(slow.ID, constants.StatusDownloading, ""))
	tracker.mu.Lock()
	tracker.deployments[slow.ID].UpdatedAt = time.Now().Add(-24 * time.Hour)
	tracker.mu.Unlock()

	tracker.CreateDeployment("trigger", "gguf", "hf", "", []string{"a"})
	assert.NotNil(t, tracker.GetDeployment(slow.ID), "a long download is not garbage")
}
