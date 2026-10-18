package prov_apps

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

type pinnedRuns struct {
	mu      sync.Mutex
	path    string
	Version int             `json:"version"`
	Runs    []EnsureRequest `json:"runs"`
}

func (m *ProviderAppManager) loadPinnedRuns() {
	s := &pinnedRuns{path: filepath.Join(config.Paths().GetDataDir(), "pinned_runs.json"), Version: 1}
	m.pins = s
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return
	}
	if err == nil {
		s.Version = 0
		err = json.Unmarshal(data, s)
	}
	if err != nil || s.Version != 1 {
		slog.Warn("Cannot load pinned runs", "path", s.path, "version", s.Version, "error", err)
		s.Runs, s.Version = nil, 1
		// Set the file aside so the next pin write cannot overwrite the evidence.
		if data != nil {
			if err := os.Rename(s.path, s.path+".corrupt"); err != nil {
				slog.Warn("Cannot set aside pinned runs", "path", s.path, "error", err)
			}
		}
	}
	for i, req := range s.Runs {
		_, svc, _ := m.resolveConfigKey(req.Provider)
		s.Runs[i].keyEndpoint = runEndpoint(svc, req.Endpoint)
	}
}

func (s *pinnedRuns) update(req EnsureRequest, pinned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.Runs, func(r EnsureRequest) bool { return r.Key() == req.Key() })
	switch {
	case pinned && i >= 0:
		if reflect.DeepEqual(s.Runs[i], req) {
			return // A recovered run keeps its place in the restore order.
		}
		s.Runs[i] = req
	case pinned:
		s.Runs = append(s.Runs, req)
	case i >= 0:
		s.Runs = slices.Delete(s.Runs, i, i+1)
	default:
		return
	}
	data, _ := json.Marshal(s) // Only strings and string maps reach the encoder.
	if err := utils.AtomicWriteFile(s.path, data, 0o600); err != nil {
		slog.Warn("Cannot persist pinned run", "provider", req.Provider, "model", req.Model, "endpoint", req.Endpoint, "error", err)
	}
}

// has reports whether a pin is recorded for key.
func (s *pinnedRuns) has(key instance.Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.Runs, func(r EnsureRequest) bool { return r.Key() == key })
}

// pinRecord is what a pin records for inst: its request, keyed as the run is.
func pinRecord(inst *instance.Instance) EnsureRequest {
	saved := restartRequest(inst)
	saved.keyEndpoint = Endpoint(inst.Endpoint)
	return saved
}

// markKept marks err kept when it refuses req's parameters while a pin is
// recorded for its key: the run then stays listed failed and pinned.
func (m *ProviderAppManager) markKept(req EnsureRequest, err error) bool {
	var refused *ParameterError
	if !errors.As(err, &refused) || !m.pins.has(req.Key()) {
		return false
	}
	refused.Kept = true
	return true
}

// keptPin reports whether err refused a pinned run that kept its pin.
func keptPin(err error) bool {
	var refused *ParameterError
	return errors.As(err, &refused) && refused.Kept
}

// RestorePinnedRuns replays each recorded request once within the manager's start drain.
func (m *ProviderAppManager) RestorePinnedRuns() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown() {
		return
	}
	m.restoreOnce.Do(func() {
		m.pins.mu.Lock()
		runs := slices.Clone(m.pins.Runs)
		m.pins.mu.Unlock()
		ctx := m.startsCtx
		m.onDemandStarts.Add(1)
		go func() {
			defer m.onDemandStarts.Done()
			for _, req := range runs {
				req.Origin = OriginRestore
				_, err := m.EnsureRun(ctx, req)
				if ctx.Err() != nil {
					return
				}
				if keptPin(err) {
					slog.Error("Pinned run refused at restore; it stays listed failed and pinned", "provider", req.Provider, "model", req.Model, "endpoint", req.Endpoint, "error", err)
					continue
				}
				if err != nil {
					slog.Warn("Cannot restore pinned run", "provider", req.Provider, "model", req.Model, "endpoint", req.Endpoint, "error", err)
					m.pins.update(req, false)
					continue
				}
				slog.Info("restoring pinned run", "provider", req.Provider, "model", req.Model, "endpoint", req.Endpoint, "origin", "restore")
			}
		}()
	})
}
