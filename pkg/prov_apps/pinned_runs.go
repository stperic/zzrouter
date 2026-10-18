package prov_apps

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/google/uuid"
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
	changed := false
	for i := range s.Runs {
		if s.Runs[i].RunID == "" {
			s.Runs[i].RunID = uuid.NewString()
			changed = true
		}
	}
	if changed {
		s.persist()
	}
}

func recordable(req EnsureRequest, lifecycle RunLifecycle) bool {
	return lifecycle == LifecycleProcess && req.DisposablePlanID == "" && req.Origin != OriginInstall
}

func (m *ProviderAppManager) replay(req EnsureRequest, lifecycle RunLifecycle) EnsureRequest {
	if !recordable(req, lifecycle) || len(req.Parameters) != 0 || len(req.EnvVars) != 0 {
		return req
	}
	cfg := m.AppsConfig()
	m.pins.mu.Lock()
	defer m.pins.mu.Unlock()
	if i := m.pins.index(pinKey(req, cfg), cfg); i >= 0 && m.pins.Runs[i].Provider == req.Provider {
		req.Parameters, req.EnvVars = maps.Clone(m.pins.Runs[i].Parameters), maps.Clone(m.pins.Runs[i].EnvVars)
	}
	return req
}

func (m *ProviderAppManager) captureRefusal(req EnsureRequest, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrShutdown) || !recordable(req, LifecycleProcess) {
		return
	}
	cfg := m.AppsConfig()
	m.pins.mu.Lock()
	defer m.pins.mu.Unlock()
	if i := m.pins.index(req.Key(), cfg); i >= 0 {
		record := &m.pins.Runs[i]
		if record.Provider == req.Provider && maps.Equal(record.Parameters, req.Parameters) && maps.Equal(record.EnvVars, req.EnvVars) {
			record.refusal = err
		}
	}
}

func (m *ProviderAppManager) runOrRecord(id string) (*instance.Instance, EnsureRequest, error) {
	if inst, ok := m.instances.Get(id); ok {
		return inst, restartRequest(inst), nil
	}
	m.pins.mu.Lock()
	defer m.pins.mu.Unlock()
	if i := slices.IndexFunc(m.pins.Runs, func(req EnsureRequest) bool { return req.RunID == id }); i >= 0 {
		req := m.pins.Runs[i]
		req.Parameters, req.EnvVars = maps.Clone(req.Parameters), maps.Clone(req.EnvVars)
		return nil, req, nil
	}
	return nil, EnsureRequest{}, ErrInstanceNotFound
}

func (s *pinnedRuns) index(key instance.Key, cfg *config.AppsConfig) int {
	return slices.IndexFunc(s.Runs, func(req EnsureRequest) bool { return pinKey(req, cfg) == key })
}

func pinKey(req EnsureRequest, cfg *config.AppsConfig) instance.Key {
	svc, _ := cfg.LookupApp(req.Provider)
	endpoint, _ := runEndpoint(svc, req.Endpoint)
	return instance.NewKey(req.Provider, req.Model, string(endpoint))
}

// update runs under mu so acceptance and forgetting observe the same registry ownership.
func (s *pinnedRuns) update(req EnsureRequest, pinned bool, cfg *config.AppsConfig) {
	i := s.index(pinKey(req, cfg), cfg)
	switch {
	case !pinned && i < 0:
		return
	case !pinned:
		s.Runs = slices.Delete(s.Runs, i, i+1)
	default:
		if i < 0 {
			s.Runs = append(s.Runs, EnsureRequest{RunID: uuid.NewString()})
			i = len(s.Runs) - 1
		}
		s.Runs[i] = EnsureRequest{
			RunID: s.Runs[i].RunID, Provider: req.Provider, Model: req.Model, Endpoint: req.Endpoint,
			Parameters: maps.Clone(req.Parameters), EnvVars: maps.Clone(req.EnvVars),
		}
	}
	s.persist()
}

func (s *pinnedRuns) persist() {
	data, _ := json.Marshal(s)
	if err := utils.AtomicWriteFile(s.path, data, 0o600); err != nil {
		slog.Warn("Cannot persist pinned run", "path", s.path, "error", err)
	}
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
				if err != nil {
					slog.Warn("Cannot restore pinned run", "provider", req.Provider, "model", req.Model, "endpoint", req.Endpoint, "error", err)
					continue
				}
				slog.Info("restored pinned run", "provider", req.Provider, "model", req.Model, "endpoint", req.Endpoint, "origin", "restore")
			}
		}()
	})
}
