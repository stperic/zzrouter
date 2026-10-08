// models_watcher.go — fsnotify-driven cache invalidation for the
// model catalog. Closes the disk-state-vs-cache drift class of bug
// where an out-of-band `rm` against the models root leaves the cache
// claiming the model is still deployed; the auto_deploy gate then
// short-circuits and a launch fires against missing files.
//
// Design notes:
//   - Two-level watch (root + immediate org dirs) covers the common
//     layout `<root>/<org>/<model>/<files>`. Mid-tree mkdir/rmdir
//     events trigger re-scan of new dirs; file-level mutations bubble
//     as parent-dir events at the org/model layer we're already
//     watching.
//   - Debounced: a burst of writes during a download or rm -rf
//     collapses to a single Invalidate. 1s window matches the
//     scan latency the cache absorbs anyway.
//   - Best-effort. Watch failures (missing root, permission errors,
//     fsnotify internal limits) log + return without bringing the
//     server down — the existing periodic refresh path remains the
//     safety net.
package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// watcherDebounce collapses rapid bursts (download writes, rm -rf
	// recursion) into a single Invalidate. Long enough to absorb a
	// directory tree teardown, short enough that an agent's next
	// /v1/models call sees the truth.
	watcherDebounce = 1 * time.Second
	// watcherDepth bounds how deep we eagerly add watches on Create
	// events. The org/model two-level pattern is universal in the
	// sources we ingest (HuggingFace, Ollama, filesystem); deeper
	// nests would be redundant since file changes bubble to the
	// model dir we already watch.
	watcherDepth = 2
)

// modelsWatcher invalidates the model cache on filesystem mutations
// under the models root. Lifecycle is server-bound (Start in the
// startup path, Stop in the shutdown path).
type modelsWatcher struct {
	root       string
	invalidate func()
	w          *fsnotify.Watcher
	cancel     context.CancelFunc
	pending    atomic.Bool
}

// newModelsWatcher constructs the watcher. Returns nil + nil when the
// root is empty (worker without a models dir, test harness) — callers
// treat nil as "feature disabled" rather than an error.
func newModelsWatcher(root string, invalidate func()) (*modelsWatcher, error) {
	if root == "" || invalidate == nil {
		return nil, nil
	}
	if _, err := os.Stat(root); err != nil {
		// Root doesn't exist yet — pre-deploy state. Skip watcher;
		// the periodic refresh will catch the eventual mkdir.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &modelsWatcher{root: root, invalidate: invalidate, w: w}, nil
}

// Start begins watching. Idempotent only via newModelsWatcher (one
// watcher per server). Cancellation flows through ctx.
func (m *modelsWatcher) Start(ctx context.Context) {
	if m == nil {
		return
	}
	wctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	if err := m.addRecursive(m.root, 0); err != nil {
		slog.Warn("models watcher: initial scan failed", "root", m.root, "error", err)
	}
	go m.run(wctx)
}

// Stop closes the watcher. Safe on a nil receiver and after a missed
// Start (NewWatcher created the channels even if Start never ran).
func (m *modelsWatcher) Stop() {
	if m == nil {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.w != nil {
		_ = m.w.Close()
	}
}

// addRecursive walks `dir` adding fsnotify watches up to watcherDepth
// levels deep relative to the watcher root. depth=0 means we're at
// the root itself.
func (m *modelsWatcher) addRecursive(dir string, depth int) error {
	if err := m.w.Add(dir); err != nil {
		return err
	}
	if depth >= watcherDepth {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		_ = m.addRecursive(filepath.Join(dir, e.Name()), depth+1)
	}
	return nil
}

// run is the event loop. Exits on ctx cancel or watcher Close.
func (m *modelsWatcher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-m.w.Events:
			if !ok {
				return
			}
			// New directory created at a watched depth: extend the
			// watch so future child events flow.
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					rel, _ := filepath.Rel(m.root, ev.Name)
					depth := 1 + len(splitPath(rel))
					if depth <= watcherDepth {
						_ = m.addRecursive(ev.Name, depth)
					}
				}
			}
			m.scheduleInvalidate(ctx)
		case err, ok := <-m.w.Errors:
			if !ok {
				return
			}
			slog.Warn("models watcher: error", "error", err)
		}
	}
}

// splitPath splits a relative path into its components. Empty/"."
// returns nil so the depth math at the call site treats them as zero.
func splitPath(rel string) []string {
	if rel == "" || rel == "." {
		return nil
	}
	return strings.Split(filepath.Clean(rel), string(filepath.Separator))
}

// scheduleInvalidate fires Invalidate on a debounced timer. Coalesces
// bursts via the pending atomic so a directory teardown collapses to
// one cache flush.
func (m *modelsWatcher) scheduleInvalidate(ctx context.Context) {
	if !m.pending.CompareAndSwap(false, true) {
		return
	}
	go func() {
		t := time.NewTimer(watcherDebounce)
		defer t.Stop()
		select {
		case <-ctx.Done():
			m.pending.Store(false)
			return
		case <-t.C:
		}
		m.pending.Store(false)
		m.invalidate()
	}()
}
