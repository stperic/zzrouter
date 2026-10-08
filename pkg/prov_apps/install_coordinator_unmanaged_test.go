package prov_apps

import (
	"context"
	"testing"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A provider zzRouter did not install still shows up in the provider
// list, so it is selectable in the TUI, whose uninstall key has never
// checked anything before firing. The refusal has to live here.
//
// It is not a formality: ollama's uninstall runs `brew uninstall` on
// darwin and removes the model directory on Windows. Applying either to
// an install we merely detected destroys something the operator owns.
func TestUninstallRefusesAProviderWeDidNotInstall(t *testing.T) {
	newCoord := func(t *testing.T) (*InstallCoordinator, *ProviderAppManager, *fakeInstaller) {
		t.Helper()
		reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
		require.NoError(t, err)
		m, err := NewProviderAppManager(testAppsConfig(), WithJobsRegistry(reg))
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = m.Stop(context.Background())
			reg.Stop()
		})
		fake := newFakeInstaller("vllm")
		m.Install().dispatcher.Register("vllm", fake)
		return m.Install(), m, fake
	}

	t.Run("detected but not ours is a refusal, not a removal", func(t *testing.T) {
		coord, m, fake := newCoord(t)
		require.False(t, fake.IsInstalled(), "no marker: zzRouter did not install this")
		m.SetProviderVersion("vllm", "0.11.0") // something is on disk

		_, err := coord.UninstallAsync(context.Background(), "vllm", nil)

		assert.ErrorIs(t, err, ErrProviderNotManaged,
			"the operator's own install must not be removable through us")
	})

	// Distinguished from the above so the caller can tell "I will not"
	// from "there is nothing there".
	t.Run("nothing on disk at all is not found", func(t *testing.T) {
		coord, _, _ := newCoord(t)

		_, err := coord.UninstallAsync(context.Background(), "vllm", nil)

		assert.ErrorIs(t, err, ErrProviderNotFound)
	})

	t.Run("our own install uninstalls normally", func(t *testing.T) {
		coord, m, fake := newCoord(t)
		fake.installed.Store(true)
		m.SetProviderVersion("vllm", "0.11.0")

		jobID, err := coord.UninstallAsync(context.Background(), "vllm", nil)

		require.NoError(t, err)
		assert.NotEmpty(t, jobID)
		fake.release()
	})
}
