package instance

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdmissionEffectsSeparateServingFromReclaimedMemory(t *testing.T) {
	run := NewInstance("tenant", "provider", "", 0, 0, 0)
	defer run.Cancel()
	run.SetAdmissionEffects(AdmissionEffects{CatalogChanged: true})
	require.True(t, run.ChangesCatalog(), "an unconfirmed model stop still changes serving evidence")
	require.Empty(t, run.EvictedRuns(), "unconfirmed stops cannot claim memory receipts")
	require.Empty(t, run.ToInfo("").EvictedRuns)

	receipts := []EvictedRun{{RunID: "victim", Model: "weights", FreedMiB: 1024}}
	run.SetAdmissionEffects(AdmissionEffects{EvictedRuns: receipts})
	receipts[0].Model = ""
	require.True(t, run.ChangesCatalog(), "caller mutation cannot erase an accepted model effect")
	view := run.EvictedRuns()
	require.Equal(t, "weights", view[0].Model)
	view[0].FreedMiB = 0
	require.Equal(t, int64(1024), run.ToInfo("").EvictedRuns[0].FreedMiB)
}
