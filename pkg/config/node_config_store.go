package config

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrEndpointExists is returned by AddClusterEndpoint when the endpoint is
// already in the cluster. Callers should match with errors.Is to render the
// appropriate user-facing response (HTTP 409 / "already in cluster" message).
var ErrEndpointExists = errors.New("node already exists in cluster")

// NodeConfigStore is the single owner of node.yaml I/O.
// All reads go through Config(); all writes go through mutation methods
// that atomically update in-memory config and persist to disk.
//
// Cluster settings and scheduled-update enablement can change at runtime.
// All other fields (node, auth, security, etc.) are read-only after startup.
//
// Subscribers register with OnChange to receive the updated config after
// every successful mutation. Listeners return a ReloadDisposition — see
// docs/plan_reload_semantics.md for the contract.
type NodeConfigStore struct {
	mu        sync.RWMutex
	cm        *ConfigManager
	cfg       *NodeConfig
	dir       string // config directory (for SaveNodeConfigToDir)
	listeners []listenerEntry
}

// listenerEntry pairs a listener callback with its registered name so
// the store can tag dispositions in the ReloadReport without forcing
// callers to carry their name into every return.
type listenerEntry struct {
	name string
	fn   func(*NodeConfig) ReloadDisposition
}

// NewNodeConfigStoreFromConfig wraps an already-loaded NodeConfig in a store.
// Used when the config was loaded earlier (e.g., by the CLI startup path)
// and we need to add centralized save protection.
func NewNodeConfigStoreFromConfig(cfg *NodeConfig) *NodeConfigStore {
	cm := NewConfigManager("zzrouter")
	return &NodeConfigStore{
		cm:  cm,
		cfg: cfg,
		dir: cm.GetNodeConfigDir(),
	}
}

// Config returns the current in-memory config. Callers must NOT mutate
// the returned value directly — use the store's mutation methods instead.
func (s *NodeConfigStore) Config() *NodeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SetUpdateEnabled persists only update.enabled and preserves state on failure.
func (s *NodeConfigStore) SetUpdateEnabled(enabled bool) error {
	s.mu.Lock()
	previous := s.cfg
	next := *previous
	next.Update.Enabled = &enabled
	s.cfg = &next
	if err := s.save(); err != nil {
		s.cfg = previous
		s.mu.Unlock()
		return err
	}
	listeners := s.snapshotListenersLocked()
	s.mu.Unlock()
	s.notify(&next, listeners, "update-enabled")
	return nil
}

// FilePath returns the path to node.yaml.
func (s *NodeConfigStore) FilePath() string {
	return s.cm.GetNodeConfigPath()
}

// OnChange registers a listener that fires after every successful mutation.
// Listeners run after the write lock is released, so they may safely call
// back into the store. They are invoked in registration order and report
// their result as a ReloadDisposition, aggregated into the per-notify
// ReloadReport logged at the notify boundary.
//
// The `name` argument tags the listener in log summaries — use the
// listener function's canonical name (e.g., "reconfigureRouter").
func (s *NodeConfigStore) OnChange(name string, fn func(*NodeConfig) ReloadDisposition) {
	s.mu.Lock()
	s.listeners = append(s.listeners, listenerEntry{name: name, fn: fn})
	s.mu.Unlock()
}

// Reload re-reads node.yaml from disk and notifies listeners. Returns
// the aggregated ReloadReport so HTTP reload handlers can surface
// per-listener dispositions to operators.
func (s *NodeConfigStore) Reload() (*ReloadReport, error) {
	s.mu.Lock()
	cfg, err := s.cm.LoadNodeConfig()
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("reload node config: %w", err)
	}
	s.cfg = cfg
	listeners := s.snapshotListenersLocked()
	s.mu.Unlock()
	return s.notify(cfg, listeners, "disk-reload"), nil
}

// save persists the current in-memory config to disk. Must be called with mu held.
func (s *NodeConfigStore) save() error {
	return s.cm.SaveNodeConfigToDir(s.cfg, s.dir)
}

// snapshotListenersLocked copies the listener slice under the lock so the
// caller can invoke listeners after releasing it.
func (s *NodeConfigStore) snapshotListenersLocked() []listenerEntry {
	if len(s.listeners) == 0 {
		return nil
	}
	out := make([]listenerEntry, len(s.listeners))
	copy(out, s.listeners)
	return out
}

// notify invokes listeners without holding the lock, aggregates their
// dispositions into a ReloadReport, and logs a summary. `source` tags
// the trigger (disk-reload, mutation name) in the summary line.
func (s *NodeConfigStore) notify(cfg *NodeConfig, listeners []listenerEntry, source string) *ReloadReport {
	report := &ReloadReport{}
	for _, l := range listeners {
		report.Add(l.name, l.fn(cfg))
	}
	report.LogSummary(source)
	return report
}

// ============================================================================
// Cluster Reads (thread-safe snapshots)
// ============================================================================

// ClusterMode returns a snapshot of cluster mode under read lock.
// Use this instead of reading s.config.Cluster.Mode directly from
// concurrent goroutines.
func (s *NodeConfigStore) ClusterMode() ClusterMode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Cluster.Mode
}

// ============================================================================
// Cluster Mutations
// ============================================================================
//
// These methods preserve explicit rollback on save failure: if the disk write
// fails, the in-memory state is reverted before the error is returned.
// Listeners only fire on successful save.

// AddClusterEndpoint appends an endpoint and sets coordinator mode. Returns error
// if the endpoint already exists. The caller should send the join request to the
// worker BEFORE calling this method — if the join fails, config is never modified.
func (s *NodeConfigStore) AddClusterEndpoint(endpoint string) error {
	s.mu.Lock()

	if s.cfg.Cluster.Endpoints.IndexOf(endpoint) >= 0 {
		s.mu.Unlock()
		return ErrEndpointExists
	}

	s.cfg.Cluster.Endpoints = append(s.cfg.Cluster.Endpoints, ClusterPeer{Address: endpoint})
	if !s.cfg.Cluster.IsMaster() {
		s.cfg.Cluster.SetMode(ClusterModeCoordinator)
	}

	if err := s.save(); err != nil {
		// Revert in-memory on save failure
		s.cfg.Cluster.Endpoints = s.cfg.Cluster.Endpoints[:len(s.cfg.Cluster.Endpoints)-1]
		if len(s.cfg.Cluster.Endpoints) == 0 {
			s.cfg.Cluster.SetMode(ClusterModeDisabled)
		}
		s.mu.Unlock()
		return err
	}
	cfg := s.cfg
	listeners := s.snapshotListenersLocked()
	s.mu.Unlock()
	s.notify(cfg, listeners, "AddClusterEndpoint")
	return nil
}

// RemoveClusterEndpoint removes an endpoint from the cluster. Reverts to
// standalone mode if no endpoints remain. Returns false if not found.
func (s *NodeConfigStore) RemoveClusterEndpoint(endpoint string) (bool, error) {
	s.mu.Lock()

	idx := s.cfg.Cluster.Endpoints.IndexOf(endpoint)
	if idx == -1 {
		s.mu.Unlock()
		return false, nil
	}

	// Save state for revert
	originalEndpoints := make(ClusterPeers, len(s.cfg.Cluster.Endpoints))
	copy(originalEndpoints, s.cfg.Cluster.Endpoints)
	originalMode := s.cfg.Cluster.Mode

	s.cfg.Cluster.Endpoints = slices.Delete(s.cfg.Cluster.Endpoints, idx, idx+1)

	if len(s.cfg.Cluster.Endpoints) == 0 && s.cfg.Cluster.IsMaster() {
		s.cfg.Cluster.SetMode(ClusterModeDisabled)
	}

	if err := s.save(); err != nil {
		s.cfg.Cluster.Endpoints = originalEndpoints
		s.cfg.Cluster.SetMode(originalMode)
		s.mu.Unlock()
		return false, err
	}
	cfg := s.cfg
	listeners := s.snapshotListenersLocked()
	s.mu.Unlock()
	s.notify(cfg, listeners, "RemoveClusterEndpoint")
	return true, nil
}

// SetClusterJoined sets this node to worker mode. Called when accepting a join
// request from a coordinator. Returns the original mode for revert on failure.
func (s *NodeConfigStore) SetClusterJoined() (originalMode ClusterMode, err error) {
	s.mu.Lock()

	originalMode = s.cfg.Cluster.Mode
	s.cfg.Cluster.SetMode(ClusterModeWorker)

	if err := s.save(); err != nil {
		s.cfg.Cluster.SetMode(originalMode)
		s.mu.Unlock()
		return originalMode, err
	}
	cfg := s.cfg
	listeners := s.snapshotListenersLocked()
	s.mu.Unlock()
	s.notify(cfg, listeners, "SetClusterJoined")
	return originalMode, nil
}

// SetClusterLeft resets this node to standalone mode. Called when accepting a
// leave request from a coordinator.
func (s *NodeConfigStore) SetClusterLeft() error {
	s.mu.Lock()

	original := s.cfg.Cluster.Mode
	s.cfg.Cluster.SetMode(ClusterModeDisabled)

	if err := s.save(); err != nil {
		s.cfg.Cluster.SetMode(original)
		s.mu.Unlock()
		return err
	}
	cfg := s.cfg
	listeners := s.snapshotListenersLocked()
	s.mu.Unlock()
	s.notify(cfg, listeners, "SetClusterLeft")
	return nil
}

// SetClusterEndpointName caches a peer's own node name against its
// address, so GET /nodes can report a usable name from the moment the
// coordinator starts instead of only after it has probed.
//
// Reports whether anything changed: the common call is a re-probe
// confirming the name already on file, and rewriting node.yaml on every
// health tick would be a write amplification for no new information.
// Unknown addresses are ignored rather than added — membership is owned
// by AddClusterEndpoint alone.
func (s *NodeConfigStore) SetClusterEndpointName(address, name string) (bool, error) {
	if address == "" || name == "" {
		return false, nil
	}

	s.mu.Lock()
	idx := s.cfg.Cluster.Endpoints.IndexOf(address)
	if idx == -1 || s.cfg.Cluster.Endpoints[idx].Name == name {
		s.mu.Unlock()
		return false, nil
	}

	previous := s.cfg.Cluster.Endpoints[idx].Name
	s.cfg.Cluster.Endpoints[idx].Name = name
	if err := s.save(); err != nil {
		s.cfg.Cluster.Endpoints[idx].Name = previous
		s.mu.Unlock()
		return false, err
	}
	cfg := s.cfg
	listeners := s.snapshotListenersLocked()
	s.mu.Unlock()
	s.notify(cfg, listeners, "SetClusterEndpointName")
	return true, nil
}
