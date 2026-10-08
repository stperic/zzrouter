package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunsExecutor_DeployGroup_CoalescesConcurrent pins the dedupe contract
// for auto_deploy: N concurrent requests for the same (provider, model, file)
// must invoke the underlying Deploy exactly once and observe the same
// Deployment result.
func TestRunsExecutor_DeployGroup_CoalescesConcurrent(t *testing.T) {
	e := &RunsExecutor{}
	const N = 8
	var calls int32
	var wg sync.WaitGroup
	results := make([]*Deployment, N)

	dedupeKey := "vllm|m|"
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, _, _ := e.deployGroup.Do(dedupeKey, func() (any, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(20 * time.Millisecond) // hold the slot so peers coalesce
				return &Deployment{ID: "deploy-1"}, nil
			})
			results[i] = v.(*Deployment)
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 underlying Deploy call, got %d", got)
	}
	for i, r := range results {
		if r == nil || r.ID != "deploy-1" {
			t.Fatalf("caller %d: expected shared Deployment{ID:deploy-1}, got %+v", i, r)
		}
	}
}

// TestRunsExecutor_DeployGroup_DistinctKeysDoNotCoalesce confirms the
// dedupe key is granular enough — different (provider, model, file)
// triples each get their own Deploy call.
func TestRunsExecutor_DeployGroup_DistinctKeysDoNotCoalesce(t *testing.T) {
	e := &RunsExecutor{}
	var calls int32
	keys := []string{"vllm|m1|", "vllm|m2|", "llamacpp|m1|Q4_K_M", "llamacpp|m1|Q8_0"}
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			_, _, _ = e.deployGroup.Do(k, func() (any, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(10 * time.Millisecond)
				return &Deployment{ID: k}, nil
			})
		}(k)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != int32(len(keys)) {
		t.Fatalf("expected %d distinct Deploy calls (one per key), got %d", len(keys), got)
	}
}
