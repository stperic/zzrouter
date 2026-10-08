package prov_apps

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// TestLaunchInstance_ConcurrentDedupe pins the race fix. Two goroutines
// call LaunchInstance for the same (provider, model) simultaneously; the
// coalescing singleflight must produce a single Instance shared by both
// callers rather than one succeeding and the other erroring with
// ErrModelAlreadyLoaded.
func TestLaunchInstance_ConcurrentDedupe(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Stop(context.Background()) })

	const callers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []*instance.Instance
		errs    []error
	)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			inst, err := m.LaunchInstance(context.Background(), LaunchRequest{
				Provider: "vllm",
				Model:    "llama3-dedupe",
			})
			mu.Lock()
			defer mu.Unlock()
			results = append(results, inst)
			errs = append(errs, err)
		}()
	}
	wg.Wait()

	// Every caller must succeed.
	for i, e := range errs {
		assert.NoError(t, e, "caller %d got an error (race should have been coalesced)", i)
	}

	// Every caller must receive the same Instance pointer — singleflight's
	// contract is that all shared callers receive the exact value the
	// winning goroutine returned.
	require.NotNil(t, results[0])
	id := results[0].ID
	for i, r := range results {
		require.NotNil(t, r, "caller %d got a nil instance", i)
		assert.Equal(t, id, r.ID, "caller %d received a different instance (%s vs %s) — dedupe broken",
			i, r.ID, id)
	}

	// Registry must carry exactly one instance for this model — the
	// smoking-gun assertion that no duplicate instance was registered.
	all := m.ListInstances("node1")
	var mineCount int
	for _, info := range all {
		if info.Model == "llama3-dedupe" {
			mineCount++
		}
	}
	assert.Equal(t, 1, mineCount, "registry has more than one instance for the deduped model")
}

// TestLaunchInstance_SequentialReturnsExisting is the sequential variant
// of the above. A second Launch after the first completes must also
// return the same running instance — the "live" fast path inside the
// singleflight body.
func TestLaunchInstance_SequentialReturnsExisting(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Stop(context.Background()) })

	first, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "vllm",
		Model:    "llama3-seq",
	})
	require.NoError(t, err)

	second, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "vllm",
		Model:    "llama3-seq",
	})
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID, "second launch should return the first instance")
}
