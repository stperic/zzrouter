package instance

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/utils"
)

const maxSanitizedModelNameLen = 32

// Registry manages all provider instances.
//
// LOCK ORDERING: Registry.mu → Instance.mu
// Always acquire Registry.mu FIRST, then Instance.mu if needed.
// NEVER acquire Instance.mu and then try to acquire Registry.mu.
type Registry struct {
	instances map[string]*Instance // instanceID → Instance
	keyMap    map[Key]string       // run key → instanceID
	portMap   map[int]string       // port → instanceID
	changed   chan struct{}
	mu        sync.RWMutex
}

// Key preserves model exclusivity while giving model-less providers their own identity.
type Key struct {
	Provider, Model, Endpoint string
}

// NewKey includes the provider only when the run serves no model.
func NewKey(provider, model, endpoint string) Key {
	if model == "" {
		return Key{Provider: provider}
	}
	if endpoint == "" {
		endpoint = "chat"
	}
	return Key{Model: model, Endpoint: endpoint}
}

// Key returns the immutable identity under which this run is registered.
func (i *Instance) Key() Key { return NewKey(i.Provider, i.Model, i.Endpoint) }

// NewRegistry creates a new instance registry.
func NewRegistry() *Registry {
	return &Registry{
		instances: make(map[string]*Instance),
		changed:   make(chan struct{}, 1),
		keyMap:    make(map[Key]string),
		portMap:   make(map[int]string),
	}
}

// Changes coalesces registry membership and instance idle-state mutations. It is never closed.
func (r *Registry) Changes() <-chan struct{} { return r.changed }

// NotifyChanged wakes observers to re-read current state; the notification is not a stop command.
func (r *Registry) NotifyChanged() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// Register adds an instance to the registry.
// Returns ErrInstanceAlreadyExists if the ID is taken,
// or ErrModelAlreadyLoaded if the model has an active instance.
func (r *Registry) Register(inst *Instance) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.instances[inst.ID]; exists {
		return fmt.Errorf("%w: %s", ErrInstanceAlreadyExists, inst.ID)
	}

	if key := inst.Key(); key != (Key{}) {
		if existingID, exists := r.keyMap[key]; exists {
			existing := r.instances[existingID]
			status := existing.GetStatus()
			if existing.StopUnconfirmed.Load() || (status != StatusFailed && status != StatusStopped) {
				return fmt.Errorf("%w: %s (instance %s)", ErrModelAlreadyLoaded, inst.Model, existingID)
			}
			// Clean up stale mappings for the replaced instance.
			delete(r.keyMap, key)
			if existing.Port > 0 {
				delete(r.portMap, existing.Port)
			}
			delete(r.instances, existingID)
		}
	}

	inst.mu.Lock()
	inst.idleChanged = r.changed
	inst.notifyIdleLocked()
	inst.mu.Unlock()
	r.instances[inst.ID] = inst
	if key := inst.Key(); key != (Key{}) {
		r.keyMap[key] = inst.ID
	}
	if inst.Port > 0 {
		r.portMap[inst.Port] = inst.ID
	}

	return nil
}

// Get returns an instance by ID.
func (r *Registry) Get(id string) (*Instance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	inst, ok := r.instances[id]
	return inst, ok
}

// GetByKey returns the run holding key.
func (r *Registry) GetByKey(key Key) (*Instance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.keyMap[key]
	if !ok {
		return nil, false
	}
	inst, ok := r.instances[id]
	return inst, ok
}

// GetByPort returns the instance using a port.
func (r *Registry) GetByPort(port int) (*Instance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	id, ok := r.portMap[port]
	if !ok {
		return nil, false
	}
	inst, ok := r.instances[id]
	return inst, ok
}

// Has checks if an instance exists.
func (r *Registry) Has(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.instances[id]
	return ok
}

// Remove removes an instance from the registry.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	inst, exists := r.instances[id]
	if !exists {
		return fmt.Errorf("%w: %s", ErrInstanceNotFound, id)
	}

	delete(r.instances, id)
	r.NotifyChanged()
	delete(r.keyMap, inst.Key())
	if inst.Port > 0 {
		delete(r.portMap, inst.Port)
	}
	return nil
}

// UpdateStatus updates an instance's status using thread-safe Mark* methods.
// The registry RLock is held throughout to prevent the instance from being
// removed mid-update.
func (r *Registry) UpdateStatus(id string, status Status, errorMsg string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	inst, exists := r.instances[id]
	if !exists {
		return fmt.Errorf("%w: %s", ErrInstanceNotFound, id)
	}

	switch status {
	case StatusFailed:
		inst.MarkFailed(errorMsg)
	case StatusRunning:
		inst.MarkRunning(true, nil)
	case StatusUnhealthy:
		inst.MarkUnhealthy(errorMsg)
	default:
		inst.mu.Lock()
		inst.setStatusLocked(status)
		inst.ErrorMessage = errorMsg
		inst.mu.Unlock()
	}
	return nil
}

// UpdateActivity applies request hints and stamps the authoritative idle state.
func (r *Registry) UpdateActivity(id string, ov *keepalive.Override) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	inst, exists := r.instances[id]
	if !exists {
		return fmt.Errorf("%w: %s", ErrInstanceNotFound, id)
	}
	inst.StampActivityWithHints(ov)
	return nil
}

// --- List methods ---

// List returns all registered instances; only the reaper ends idle runs.
func (r *Registry) List() []*Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Instance, 0, len(r.instances))
	for _, inst := range r.instances {
		result = append(result, inst)
	}
	return result
}

// ListRunning returns all instances with status Running or Unhealthy.
func (r *Registry) ListRunning() []*Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*Instance
	for _, inst := range r.instances {
		s := inst.GetStatus()
		if s == StatusRunning || s == StatusUnhealthy {
			result = append(result, inst)
		}
	}
	return result
}

// ListByProvider returns all instances for a provider.
func (r *Registry) ListByProvider(provider string) []*Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*Instance
	for _, inst := range r.instances {
		if inst.Provider == provider {
			result = append(result, inst)
		}
	}
	return result
}

// CountByProvider returns the number of instances for a provider.
func (r *Registry) CountByProvider(provider string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, inst := range r.instances {
		if inst.Provider == provider {
			count++
		}
	}
	return count
}

// CountActiveByProvider returns the number of non-terminal instances for a provider.
// Terminal states are excluded unless process exit is still unconfirmed.
func (r *Registry) CountActiveByProvider(provider string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, inst := range r.instances {
		if inst.Provider == provider && (inst.StopUnconfirmed.Load() || !inst.GetStatus().IsTerminal()) {
			count++
		}
	}
	return count
}

// CleanupExpiredFailures removes failed instances older than FailedInstanceTTL.
// Returns the number of removed instances.
func (r *Registry) CleanupExpiredFailures() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := utils.Now()
	removed := 0

	for id, inst := range r.instances {
		inst.mu.RLock()
		status := inst.Status
		failedAt := inst.FailedAt
		inst.mu.RUnlock()

		if status == StatusFailed && !inst.StopUnconfirmed.Load() && !failedAt.IsZero() && now.Sub(failedAt) > FailedInstanceTTL {
			delete(r.instances, id)
			r.NotifyChanged()
			delete(r.keyMap, inst.Key())
			if inst.Port > 0 {
				delete(r.portMap, inst.Port)
			}
			removed++
		}
	}
	return removed
}

// Stats returns registry statistics.
func (r *Registry) Stats() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()

	counts := map[string]int{
		"total":      len(r.instances),
		"starting":   0,
		"running":    0,
		"unhealthy":  0,
		"stopping":   0,
		"stopped":    0,
		"failed":     0,
		"restarting": 0,
	}

	for _, inst := range r.instances {
		switch inst.GetStatus() {
		case StatusStarting:
			counts["starting"]++
		case StatusRunning:
			counts["running"]++
		case StatusUnhealthy:
			counts["unhealthy"]++
		case StatusStopping:
			counts["stopping"]++
		case StatusStopped:
			counts["stopped"]++
		case StatusFailed:
			counts["failed"]++
		case StatusRestarting:
			counts["restarting"]++
		}
	}

	return map[string]any{"instances": counts}
}

// --- ID generation ---

// idCounter provides monotonically increasing uniqueness across rapid calls,
// supplementing utils.Now().UnixNano() on platforms with limited clock resolution.
var idCounter atomic.Uint64

// GenerateID generates a unique instance ID (12-char hex hash).
// Endpoint enters the seed so rapid (model, chat) and (model, embeddings)
// launches in the same nanosecond cannot collide.
func GenerateID(provider, model, endpoint string) string {
	if endpoint == "" {
		endpoint = "chat"
	}
	sanitized := sanitizeModelName(model)
	seq := idCounter.Add(1)
	source := fmt.Sprintf("%s-%s-%s-%d-%d", provider, sanitized, endpoint, utils.Now().UnixNano(), seq)
	hash := sha256.Sum256([]byte(source))
	return fmt.Sprintf("%x", hash[:6])
}

func sanitizeModelName(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, ":", "-")
	name = strings.ReplaceAll(name, " ", "-")
	name = strings.ToLower(name)
	if len(name) > maxSanitizedModelNameLen {
		name = name[:maxSanitizedModelNameLen]
	}
	return name
}

// Sentinel errors for the instance package.
// These are the canonical definitions. The parent prov_apps package
// re-exports aliases pointing to these values.
var (
	ErrInstanceAlreadyExists = errors.New("instance already registered")
	ErrModelAlreadyLoaded    = errors.New("model already has a running instance")
	ErrInstanceNotFound      = errors.New("instance not found")
	ErrAtCapacity            = errors.New("instance at maximum concurrent request capacity")
)
