package config

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppsConfigStorePublishesTransactionsInOrder(t *testing.T) {
	for _, operation := range []string{"mutate", "asset", "provider-sync", "reload"} {
		t.Run(operation, func(t *testing.T) {
			store, _ := durabilityConfig(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var wg sync.WaitGroup
			defer func() { unblock(); wg.Wait() }()
			var calls atomic.Int32
			var orderMu sync.Mutex
			var order []int32
			store.OnChange("ordered", func(cfg *AppsConfig) ReloadDisposition {
				call := calls.Add(1)
				if call == 1 {
					close(entered)
					<-release
				}
				assert.Same(t, cfg, store.Config(), "callbacks can read the published snapshot without racing the next transaction")
				orderMu.Lock()
				order = append(order, call)
				orderMu.Unlock()
				return DispositionApplied
			})
			firstDone := make(chan struct{})
			wg.Go(func() {
				defer close(firstDone)
				assert.NoError(t, store.ApplyParameterPatch("a", func(s *ServiceConfig) error { s.Description = "first"; return nil }))
			})
			waitPublication(t, entered)
			secondStarted, secondDone := make(chan struct{}), make(chan struct{})
			wg.Go(func() {
				defer close(secondDone)
				close(secondStarted)
				switch operation {
				case "mutate":
					_, err := store.SetProviderEnabled("a", "", true)
					assert.NoError(t, err)
				case "asset":
					_, err := store.WriteAsset("a", "ordering.txt", []byte("test asset"))
					assert.NoError(t, err)
				case "provider-sync":
					app := *store.Config().GetCloud("a")
					app.Description = "second"
					body, err := marshalProviderBody(&app)
					if assert.NoError(t, err) {
						_, err = store.WriteProviderFiles("a", KindCloud, ProviderFiles{Config: body})
						assert.NoError(t, err)
					}
				case "reload":
					_, err := store.Reload()
					assert.NoError(t, err)
				}
			})
			waitPublication(t, secondStarted)
			deadline := time.NewTimer(100 * time.Millisecond)
			defer deadline.Stop()
			select {
			case <-secondDone:
				t.Error("the newer transaction completed before the older callback finished")
			case <-deadline.C:
			}
			unblock()
			waitPublication(t, firstDone)
			waitPublication(t, secondDone)
			orderMu.Lock()
			defer orderMu.Unlock()
			require.Equal(t, []int32{1, 2}, order)
		})
	}
}

func waitPublication(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("provider publication did not complete")
	}
}
