package cache

import (
	"sync"
	"testing"
	"time"
)

// TestCache_ConcurrentLookupAndInvalidate is a race-detector test
// (go test -race). It hammers LookupByName / HasModel / GetAllModels
// while Invalidate + updateModelCache churn state under them.
// If the read lock discipline is broken, -race will report.
//
// No correctness assertions — the goal is to catch unsynchronized
// field access that could cause corruption in production.
func TestCache_ConcurrentLookupAndInvalidate(t *testing.T) {
	c := newWorkerCache(t)
	c.updateModelCache(samplesFor())

	const readers = 8
	const writers = 2
	done := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				_, _ = c.LookupByName("llama3")
				_ = c.HasModel("qwen/qwen2.5-7b")
				_ = c.GetAllModels()
				_ = c.GetCacheStats()
			}
		}()
	}

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				c.Invalidate()
				c.updateModelCache(samplesFor())
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(done)
	wg.Wait()
}

// TestCache_StartStop_ConcurrentWithNotify exercises the invariant
// that the notifyWG Add-under-lock gate prevents Add-after-Wait UB.
// A burst of notify calls races a concurrent Stop. Without the lock
// gate, this would occasionally panic with "sync: negative
// WaitGroup counter" or leak goroutines past Stop.
