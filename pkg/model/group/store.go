package group

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"sync"

	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
	"github.com/stperic/zzrouter/pkg/utils"
	"gopkg.in/yaml.v3"
)

// ErrGroupNotFound is returned by ApplyPatch when the named group is absent.
// Callers can errors.Is-match to produce a 404 with closed-enum body.
var ErrGroupNotFound = errors.New("model group not found")

// ErrReplicaNotFound is returned by replica-level mutators when the named
// replica is absent in the target group.
var ErrReplicaNotFound = errors.New("replica not found")

// ErrReplicaLastInGroup is returned by DeleteReplica when removing the
// replica would leave the group empty. The mutator path treats empty
// replicas as invalid even though the loader still tolerates them (for
// the create-group-first / add-replicas-later workflow).
var ErrReplicaLastInGroup = errors.New("replica is the last in group")

// ErrRouteNotClaimed is returned when releasing ownership of a group that
// was never generator-created. Only a claimed auto-route carries an
// AutoOwner; a hand-authored group has nothing to release, and flipping it
// to AutoManaged would hand the generator a name it does not own.
var ErrRouteNotClaimed = errors.New("model group is not a claimed auto-route")

// GroupStore provides thread-safe access to model group definitions.
type GroupStore struct {
	mu         sync.RWMutex
	groups     map[string]ModelGroup
	path       string            // File path for persistence (set by SetPath or LoadFromFile)
	isCloudApp func(string) bool // Returns true if the app name is a cloud provider (optional)
	events     *route_events.Bus // Optional lifecycle event publisher (nil = no publish)
}

// NewGroupStore creates an empty GroupStore.
func NewGroupStore() *GroupStore {
	return &GroupStore{
		groups: make(map[string]ModelGroup),
	}
}

// SetCloudAppChecker sets the function used to determine if an app is a cloud provider.
// Cloud provider replicas have their Node field stripped on save, since requests
// always go to the cloud API endpoint regardless of node.
func (s *GroupStore) SetCloudAppChecker(fn func(string) bool) {
	s.isCloudApp = fn
}

// SetEventBus wires a route_events.Bus into the store so Set / Delete /
// ApplyPatch fan lifecycle events out to SSE subscribers. Optional —
// passing nil (or never calling this) is a clean no-op.
func (s *GroupStore) SetEventBus(bus *route_events.Bus) {
	s.events = bus
}

// publish is the internal nil-safe sender. Inlined sites keep the
// publish surface narrow and the lock window short.
func (s *GroupStore) publish(et route_events.EventType, name string, detail map[string]string) {
	if s.events == nil {
		return
	}
	s.events.Publish(route_events.Event{Type: et, Route: name, Detail: detail})
}

// LoadFromFile reads and parses a model_groups.yaml file into the store.
// It replaces all existing groups with the file contents.
func (s *GroupStore) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read model groups config: %w", err)
	}
	s.mu.Lock()
	s.path = path
	s.mu.Unlock()
	return s.LoadFromBytes(data)
}

// LoadFromBytes parses YAML bytes into the store.
func (s *GroupStore) LoadFromBytes(data []byte) error {
	var cfg modelGroupsFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse model groups config: %w", err)
	}

	if cfg.Version == "" {
		return fmt.Errorf("model groups config missing required 'version' field")
	}

	for name, group := range cfg.ModelGroups {
		if err := validateGroup(name, &group); err != nil {
			return err
		}
		sort.Slice(group.Replicas, func(i, j int) bool {
			return group.Replicas[i].Priority < group.Replicas[j].Priority
		})
		cfg.ModelGroups[name] = group
	}

	s.mu.Lock()
	s.groups = cfg.ModelGroups
	if s.groups == nil {
		s.groups = make(map[string]ModelGroup)
	}
	s.mu.Unlock()

	return nil
}

// Get returns the model group for the given name, or nil if not found.
func (s *GroupStore) Get(name string) *ModelGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	group, ok := s.groups[name]
	if !ok {
		return nil
	}
	return &group
}

// List returns all model group names and their groups.
func (s *GroupStore) List() map[string]ModelGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]ModelGroup, len(s.groups))
	maps.Copy(result, s.groups)
	return result
}

// Names returns a sorted list of all group names.
func (s *GroupStore) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.groups))
	for name := range s.groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Len returns the number of groups in the store.
func (s *GroupStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.groups)
}

// Set adds or replaces a model group. Validates and sorts replicas by priority.
// Publishes route_created or route_updated (whichever matches the pre-image)
// when an event bus is wired.
func (s *GroupStore) Set(name string, group ModelGroup) error {
	if err := validateGroup(name, &group); err != nil {
		return err
	}

	// Strip node from cloud provider replicas — node is only meaningful for local apps
	if s.isCloudApp != nil {
		for i := range group.Replicas {
			if group.Replicas[i].Node != "" && s.isCloudApp(group.Replicas[i].App) {
				group.Replicas[i].Node = ""
			}
		}
	}

	sort.Slice(group.Replicas, func(i, j int) bool {
		return group.Replicas[i].Priority < group.Replicas[j].Priority
	})

	s.mu.Lock()
	_, existed := s.groups[name]
	s.groups[name] = group
	s.mu.Unlock()

	if existed {
		s.publish(route_events.EventRouteUpdated, name, nil)
	} else {
		s.publish(route_events.EventRouteCreated, name, nil)
	}
	return nil
}

// ApplyPatch atomically reads, mutates, validates, and replaces the named
// group under the store's write lock so concurrent writers can't lose state
// between the read and the write. mutate receives a value copy of the
// current group and returns the desired replacement or an error to abort
// (no state change). Validation mirrors Set (strategy enum, replica
// uniqueness). Returns ErrGroupNotFound when the name doesn't exist.
//
// Publishes route_updated and any replica_added / replica_removed events
// implied by the patch *after* releasing the lock so a slow subscriber
// can't backpressure concurrent mutators. The diff is computed under
// the lock from the pre-image; emission is deferred.
func (s *GroupStore) ApplyPatch(name string, mutate func(current ModelGroup) (ModelGroup, error)) error {
	s.mu.Lock()
	current, ok := s.groups[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrGroupNotFound, name)
	}
	patched, err := mutate(current)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := validateGroup(name, &patched); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.isCloudApp != nil {
		for i := range patched.Replicas {
			if patched.Replicas[i].Node != "" && s.isCloudApp(patched.Replicas[i].App) {
				patched.Replicas[i].Node = ""
			}
		}
	}
	sort.Slice(patched.Replicas, func(i, j int) bool {
		return patched.Replicas[i].Priority < patched.Replicas[j].Priority
	})
	s.groups[name] = patched
	added, removed := diffReplicaNames(current.Replicas, patched.Replicas)
	s.mu.Unlock()

	s.publish(route_events.EventRouteUpdated, name, nil)
	for _, r := range added {
		s.publish(route_events.EventReplicaAdded, name, map[string]string{"replica": r})
	}
	for _, r := range removed {
		s.publish(route_events.EventReplicaRemoved, name, map[string]string{"replica": r})
	}
	return nil
}

// diffReplicaNames returns (added, removed) name sets between two
// replica lists. Order-independent; runs in O(n+m) via a single map.
func diffReplicaNames(before, after []Replica) (added, removed []string) {
	beforeSet := make(map[string]bool, len(before))
	for _, r := range before {
		beforeSet[r.Name] = true
	}
	afterSet := make(map[string]bool, len(after))
	for _, r := range after {
		afterSet[r.Name] = true
		if !beforeSet[r.Name] {
			added = append(added, r.Name)
		}
	}
	for _, r := range before {
		if !afterSet[r.Name] {
			removed = append(removed, r.Name)
		}
	}
	return added, removed
}

// Delete removes a model group. Returns false if it didn't exist.
// Publishes route_deleted on success when an event bus is wired.
func (s *GroupStore) Delete(name string) bool {
	return s.deleteWithEvent(name, route_events.EventRouteDeleted)
}

// expireGroup removes an ephemeral group whose TTL elapsed and
// publishes a route_expired event so dashboards can distinguish
// server-initiated cleanup from user-initiated deletes. Internal —
// only the reaper calls it.
func (s *GroupStore) expireGroup(name string) bool {
	return s.deleteWithEvent(name, route_events.EventRouteExpired)
}

func (s *GroupStore) deleteWithEvent(name string, et route_events.EventType) bool {
	s.mu.Lock()
	if _, ok := s.groups[name]; !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.groups, name)
	s.mu.Unlock()
	s.publish(et, name, nil)
	return true
}

// Path returns the configured file path.
func (s *GroupStore) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// Save persists model groups to the configured file path.
func (s *GroupStore) Save() error {
	path := s.Path()
	if path == "" {
		return fmt.Errorf("no file path configured")
	}
	return s.SaveToFile(path)
}

// SetPath stores the file path for SaveToFile.
func (s *GroupStore) SetPath(path string) {
	s.mu.Lock()
	s.path = path
	s.mu.Unlock()
}

// SaveToFile serializes snapshot capture through completed persistence so
// concurrent saves cannot publish stale state or overlap Windows replacement.
func (s *GroupStore) SaveToFile(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := modelGroupsFile{
		Version:     "1",
		ModelGroups: s.groups,
	}
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal model groups: %w", err)
	}

	return utils.AtomicWriteFile(path, data, 0o644)
}

// validateGroup checks that a model group definition is valid.
func validateGroup(name string, group *ModelGroup) error {
	// Allow empty replicas — routes can be created first, replicas added later

	// Default strategy to priority, validate known values
	if group.Strategy == "" {
		group.Strategy = StrategyPriority
	}
	switch group.Strategy {
	case StrategyPriority, StrategyLeastLoad, StrategyFastest:
		// valid
	default:
		return fmt.Errorf("model group %q has unknown strategy %q (must be priority, least-load, or fastest)", name, group.Strategy)
	}

	seen := make(map[string]bool)
	for i, rep := range group.Replicas {
		if rep.Name == "" {
			return fmt.Errorf("model group %q replica %d has no name", name, i)
		}
		if rep.Model == "" {
			return fmt.Errorf("model group %q replica %q has no model", name, rep.Name)
		}
		if rep.App == "" {
			return fmt.Errorf("model group %q replica %q has no app", name, rep.Name)
		}
		if seen[rep.Name] {
			return fmt.Errorf("model group %q has duplicate replica name %q", name, rep.Name)
		}
		seen[rep.Name] = true
	}

	return nil
}
