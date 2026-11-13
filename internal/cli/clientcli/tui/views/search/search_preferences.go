package search

import (
	"sync"

	tea "charm.land/bubbletea/v2"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/observability/logger"
)

type searchPreferenceStore interface {
	SetSearchPrefs(registry, sort string, tags []string) error
	SetModelTags([]pkgConfig.TagFilter) error
}

// searchPreferenceWriter preserves UI event order independently of Tea command
// scheduling. Its lifetime is the search model's, not one activation: accepted
// preference changes still save after navigation cancels network observation.
type searchPreferenceWriter struct {
	queueMu sync.Mutex
	pending []func(searchPreferenceStore) error

	// Disk work never holds queueMu, so Update can enqueue without waiting for I/O.
	writeMu sync.Mutex
	store   searchPreferenceStore
}

func (w *searchPreferenceWriter) enqueue(change func(searchPreferenceStore) error) tea.Cmd {
	w.queueMu.Lock()
	w.pending = append(w.pending, change)
	w.queueMu.Unlock()
	return func() tea.Msg {
		if err := w.flush(); err != nil {
			logger.Warn("Search preferences could not be saved", "error", err)
		}
		return nil
	}
}

func (w *searchPreferenceWriter) flush() error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	for {
		w.queueMu.Lock()
		if len(w.pending) == 0 {
			w.queueMu.Unlock()
			return nil
		}
		change := w.pending[0]
		w.queueMu.Unlock()

		if w.store == nil {
			store, err := pkgConfig.NewClientConfigStore(pkgConfig.NewConfigManager("zzrouter"))
			if err != nil {
				return err
			}
			w.store = store
		}
		if err := change(w.store); err != nil {
			// Keep this change and every later one in order. A later queued
			// command retries; navigation never discards the pending queue.
			return err
		}
		w.queueMu.Lock()
		w.pending[0] = nil
		w.pending = w.pending[1:]
		w.queueMu.Unlock()
	}
}
