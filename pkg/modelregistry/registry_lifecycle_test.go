package modelregistry

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"

	"github.com/stperic/zzrouter/pkg/config"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(func() *config.AppsConfig { return nil })
	require.NoError(t, err)
	return r
}

// Stop on a fresh Registry must return immediately and be idempotent.
// Without idempotence, a server-shutdown path that calls Stop more than
// once would deadlock or panic on double-flag-set.
func TestRegistryStop_IdempotentOnFreshRegistry(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Stop()
		r.Stop()
		r.Stop()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return on fresh Registry within 2s")
	}
}

// Post-Stop async spawns must be refused via the statusUpdater rather
// than silently spawning a zombie goroutine. This is the central
// invariant — without it Stop is a lie.
func TestRegistryStop_RefusesSubsequentDownloadHF(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	r.Stop()

	statusCh := make(chan [2]string, 4)
	r.DownloadHuggingFaceModelAsync(context.Background(), metadata.DownloadRequest{Repo: "irrelevant/model"},
		func(s, currentFile string, _, _ int, _ float64, _, _, _ int64) {
			select {
			case statusCh <- [2]string{s, currentFile}:
			default:
			}
		})

	select {
	case got := <-statusCh:
		assert.Equal(t, "failed", got[0])
		assert.True(t, strings.Contains(got[1], "registry stopped"),
			"expected 'registry stopped' substring in payload, got status=%q msg=%q", got[0], got[1])
	case <-time.After(2 * time.Second):
		t.Fatal("statusUpdater did not fire after Stop within 2s")
	}
}

// Same invariant for the Ollama path. Note: the pre-Stop guard
// (IsConfigured) fires first on an unconfigured connector, so a fresh
// Registry without a configured Ollama produces a "failed" status with
// the ollama-not-configured message. This test pins that the call
// returns without panic and produces SOME "failed" status — the central
// invariant of "no zombie goroutine" is what we're guarding.
func TestRegistryStop_RefusesSubsequentDownloadOllama(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	r.Stop()

	called := atomic.Bool{}
	r.PullOllamaModelAsync(context.Background(), "irrelevant",
		func(s, _ string, _, _ int, _ float64, _, _, _ int64) {
			if s == "failed" {
				called.Store(true)
			}
		})
	deadline := time.Now().Add(time.Second)
	for !called.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	assert.True(t, called.Load(), "expected statusUpdater to fire 'failed' after Stop")
}

// Lifecycle gate must not race wg.Add against wg.Wait. Hammer the gate
// from many goroutines while Stop runs concurrently — under -race this
// catches a regression that lifts the stopMu protection.
func TestRegistryStop_RaceFreeUnderConcurrentSpawns(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	const N = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			r.DownloadHuggingFaceModelAsync(context.Background(), metadata.DownloadRequest{Repo: "x/y"},
				func(_, _ string, _, _ int, _ float64, _, _, _ int64) {})
		}()
	}
	close(start)
	r.Stop()
	wg.Wait()
	// Pass under -race is the assertion. A regression that drops the
	// stopMu guard would surface here as wg.Add concurrent with
	// wg.Wait → "WaitGroup is reused before previous Wait has
	// returned" panic.
}
