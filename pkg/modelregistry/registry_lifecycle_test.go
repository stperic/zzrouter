package modelregistry

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
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

// Post-Stop synchronous work returns its failure without invoking progress.
func TestRegistryStop_RefusesSubsequentDownloadHF(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	r.Stop()

	err := r.DownloadHuggingFaceModel(context.Background(), metadata.DownloadRequest{Repo: "irrelevant/model"},
		func(string, string, int, int, float64, int64, int64, int64) {
			t.Error("refused synchronous work must return its failure to the caller")
		}, r.CatalogDestination("irrelevant/model"))
	require.ErrorIs(t, err, ErrRegistryStopped)
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
	r.PullOllamaModel(context.Background(), "irrelevant",
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
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = r.DownloadHuggingFaceModel(ctx, metadata.DownloadRequest{Repo: "x/y"},
				func(_, _ string, _, _ int, _ float64, _, _, _ int64) {}, r.CatalogDestination("x/y"))
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

type lifecycleTransport func(*http.Request) (*http.Response, error)

func (f lifecycleTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRegistryStopWaitsForSynchronousHuggingFaceCleanup(t *testing.T) {
	r := newTestRegistry(t)
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	r.GetHuggingFaceConnector().SetHTTPTransport(lifecycleTransport(func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-req.Context().Done()
		close(cancelled)
		<-release
		return nil, req.Context().Err()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- r.DownloadHuggingFaceModel(ctx, metadata.DownloadRequest{Repo: "org/repo"},
			func(string, string, int, int, float64, int64, int64, int64) {
				t.Error("failed work must return its error")
			}, r.CatalogDestination("org/repo"))
	}()
	<-entered
	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()
	require.Eventually(t, func() bool {
		r.stopMu.Lock()
		defer r.stopMu.Unlock()
		return r.stopped
	}, time.Second, time.Millisecond)
	cancel()
	<-cancelled
	select {
	case <-result:
		t.Fatal("synchronous call returned before cleanup")
	case <-stopped:
		t.Fatal("Stop returned before cleanup")
	default:
	}
	unblock()
	require.ErrorIs(t, <-result, context.Canceled)
	<-stopped
}

// Deleting a split GGUF removes every shard and nothing else.
func TestDeleteGGUFVariant_RemovesEveryShard(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	dir := t.TempDir()
	for _, name := range []string{"m-q4-00001-of-00002.gguf", "m-q4-00002-of-00002.gguf", "m-q2.gguf"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
	}

	target, err := r.SelectDeletion(&metadata.ModelMetadata{FullPath: filepath.Join(dir, "m-q4-00001-of-00002.gguf"), Format: metadata.FormatGGUF})
	require.NoError(t, err)
	changed, err := r.DeleteSelected(t.Context(), target)
	require.NoError(t, err)
	require.True(t, changed)
	left, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, left, 1)
	assert.Equal(t, "m-q2.gguf", left[0].Name())
}
