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
	modelMap  map[string]string    // modelEndpointKey(model, endpoint) → instanceID
	portMap   map[int]string       // port → instanceID
	mu        sync.RWMutex
}

// modelEndpointKey composes the lookup key. NUL separator is invalid in
// any model/endpoint identifier so collision is impossible.
func modelEndpointKey(model, endpoint string) string {
	if endpoint == "" {
		endpoint = "chat"
	}
	return model + "\x00" + endpoint
}

// NewRegistry creates a new instance registry.
func NewRegistry() *Registry {
	return &Registry{
		instances: make(map[string]*Instance),
		modelMap:  make(map[string]string),
		portMap:   make(map[int]string),
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

	if inst.Model != "" {
		key := modelEndpointKey(inst.Model, inst.Endpoint)
		if existingID, exists := r.modelMap[key]; exists {
			existing := r.instances[existingID]
			status := existing.GetStatus()
			if existing.StopUnconfirmed.Load() || (status != StatusFailed && status != StatusStopped) {
				return fmt.Errorf("%w: %s (instance %s)", ErrModelAlreadyLoaded, inst.Model, existingID)
			}
			// Clean up stale mappings for the replaced instance.
			delete(r.modelMap, key)
			if existing.Port > 0 {
				delete(r.portMap, existing.Port)
			}
			delete(r.instances, existingID)
		}
	}

	r.instances[inst.ID] = inst
	if inst.Model != "" {
		r.modelMap[modelEndpointKey(inst.Model, inst.Endpoint)] = inst.ID
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

// GetByModel returns the chat instance for a model. Shim over
// GetByModelEndpoint(model, "chat") for callers that don't carry
// endpoint context.
func (r *Registry) GetByModel(model string) (*Instance, bool) {
	return r.GetByModelEndpoint(model, "chat")
}

// GetByModelEndpoint returns the instance serving (model, endpoint).
func (r *Registry) GetByModelEndpoint(model, endpoint string) (*Instance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	id, ok := r.modelMap[modelEndpointKey(model, endpoint)]
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
	if inst.Model != "" {
		delete(r.modelMap, modelEndpointKey(inst.Model, inst.Endpoint))
	}
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
		inst.MarkRunning()
	case StatusUnhealthy:
		inst.MarkUnhealthy(errorMsg)
	case StatusStarting:
		inst.MarkStarting()
	default:
		inst.mu.Lock()
		inst.Status = status
		inst.ErrorMessage = errorMsg
		inst.mu.Unlock()
	}
	return nil
}

// UpdateActivity updates the last activity time for an instance and applies
// any non-nil per-request hints (e.g. keep_alive override). Passing a nil
// hints pointer preserves the prior behavior: timestamps are stamped and
// the keep-alive timer is reset using the instance's configured value.
//
// Both LastActivity and lastUsedAt are set atomically under a single lock
// (inside Instance.StampActivityWithHints) to prevent TOCTOU inconsistencies
// in IsIdle/IsExpired.
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

// List returns all non-expired instances.
func (r *Registry) List() []*Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := utils.Now()
	result := make([]*Instance, 0, len(r.instances))
	for _, inst := range r.instances {
		if !inst.IsExpired(now) {
			result = append(result, inst)
		}
	}
	return result
}

// ListAll returns all instances including expired ones.
func (r *Registry) ListAll() []*Instance {
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
			if inst.Model != "" {
				delete(r.modelMap, modelEndpointKey(inst.Model, inst.Endpoint))
			}
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

// GenerateShortHash produces a 12-char hex hash from an arbitrary source string.
// Used for pseudo-instance IDs (e.g., endpoint provider running models).
func GenerateShortHash(source string) string {
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
