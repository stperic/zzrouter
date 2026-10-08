package mesh

import (
	"fmt"
	"strings"
	"sync"

	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/utils"
)

// livenessThreshold is the number of consecutive failed probes before an
// endpoint is evicted from UP/DEGRADED to DOWN. With the default 30s probe
// cadence this gives ~60s grace to a worker that misses a single probe —
// covering Wi-Fi micro-drops, coord GC pauses, and router blips without
// removing the peer from dispatch.
const livenessThreshold = 2

// EndpointRegistry manages cluster endpoints with O(1) lookups by alias/name/nodename.
// Per-endpoint health hysteresis lives in a parallel liveness map keyed by URL,
// mutated only through Observe.
type EndpointRegistry struct {
	endpoints map[string]*Endpoint
	liveness  map[string]*Liveness

	// Reverse indexes: lowercase key → endpoint URL (for O(1) targeted lookups)
	byAlias    map[string]string
	byName     map[string]string
	byNodeName map[string]string

	// Self-slot: the coord's own view of itself. Always IsLocal=true,
	// always Status=StatusUp, bypasses the Liveness state machine. Written
	// via ObserveLocal (not through the probe path) and never evicted.
	// Separate field rather than sharing the endpoints map so GetAll*
	// dispatch logic stays unaffected.
	local *Endpoint

	mu sync.RWMutex
}

// NewEndpointRegistry creates a new endpoint registry
func NewEndpointRegistry() *EndpointRegistry {
	return &EndpointRegistry{
		endpoints:  make(map[string]*Endpoint),
		liveness:   make(map[string]*Liveness),
		byAlias:    make(map[string]string),
		byName:     make(map[string]string),
		byNodeName: make(map[string]string),
	}
}

// RegisterEndpoint registers an endpoint
func (r *EndpointRegistry) RegisterEndpoint(endpoint *Endpoint) error {
	if endpoint.URL == "" {
		return fmt.Errorf("endpoint URL cannot be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.endpoints[endpoint.URL] = endpoint
	r.liveness[endpoint.URL] = NewLiveness(livenessThreshold)
	r.indexEndpoint(endpoint)
	return nil
}

// ObserveLocal writes the coord's own EndpointSnapshot to the self-slot.
// First call registers it; later calls update. Always IsLocal=true and
// Status=StatusUp — the self-slot cannot fail relative to itself.
func (r *EndpointRegistry) ObserveLocal(snap EndpointSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.local == nil {
		r.local = &Endpoint{IsLocal: true, Status: StatusUp}
	}
	r.local.Snapshot = snap
	r.local.Status = StatusUp
}

// Self returns a deep copy of the coord's self-slot, or nil if ObserveLocal
// has not yet been called.
func (r *EndpointRegistry) Self() *Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.local == nil {
		return nil
	}
	return copyEndpoint(r.local)
}

// ErrSelfEvictionForbidden is returned when a caller tries to unregister
// the coord's own self-slot. Per the cache-coherency contract the self
// slot cannot be evicted — it's not a peer.
var ErrSelfEvictionForbidden = fmt.Errorf("self-slot cannot be unregistered")

// UnregisterEndpoint removes an endpoint. Refuses to touch the self-slot.
func (r *EndpointRegistry) UnregisterEndpoint(url string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.local != nil && r.local.URL == url && url != "" {
		return ErrSelfEvictionForbidden
	}

	ep, exists := r.endpoints[url]
	if !exists {
		return fmt.Errorf("endpoint not found: %s", url)
	}

	r.deindexEndpoint(ep)
	delete(r.endpoints, url)
	delete(r.liveness, url)
	return nil
}

// indexEndpoint adds the endpoint's fields to the reverse lookup maps.
// Must be called with mu held for writing.
func (r *EndpointRegistry) indexEndpoint(ep *Endpoint) {
	if ep.Alias != "" {
		r.byAlias[strings.ToLower(ep.Alias)] = ep.URL
	}
	if ep.Name != "" {
		r.byName[strings.ToLower(ep.Name)] = ep.URL
	}
	if ep.NodeName != "" {
		r.byNodeName[strings.ToLower(ep.NodeName)] = ep.URL
	}
}

// deindexEndpoint removes the endpoint's fields from the reverse lookup maps.
// Only deletes if the index still points to this endpoint (guards against alias collisions).
// Must be called with mu held for writing.
func (r *EndpointRegistry) deindexEndpoint(ep *Endpoint) {
	if ep.Alias != "" {
		key := strings.ToLower(ep.Alias)
		if r.byAlias[key] == ep.URL {
			delete(r.byAlias, key)
		}
	}
	if ep.Name != "" {
		key := strings.ToLower(ep.Name)
		if r.byName[key] == ep.URL {
			delete(r.byName, key)
		}
	}
	if ep.NodeName != "" {
		key := strings.ToLower(ep.NodeName)
		if r.byNodeName[key] == ep.URL {
			delete(r.byNodeName, key)
		}
	}
}

// copyEndpoint returns a deep copy of an Endpoint.
// Callers receive an independent snapshot that is safe to read and mutate
// without holding the registry lock.
func copyEndpoint(ep *Endpoint) *Endpoint {
	cp := *ep
	// Struct copy above duplicates scalar fields; Apps is a typed slice
	// whose backing array aliases the registry's storage — clone it.
	// Nested provider diagnostics must not alias another caller's snapshot.
	if ep.Snapshot.Apps != nil {
		cloned := make([]prov_apps.LocalProviderInfo, len(ep.Snapshot.Apps))
		copy(cloned, ep.Snapshot.Apps)
		for i := range cloned {
			cloned[i].Formats = append([]string(nil), cloned[i].Formats...)
			if cloned[i].Service != nil {
				status := cloned[i].Service.Clone()
				cloned[i].Service = &status
			}
		}
		cp.Snapshot.Apps = cloned
	}
	if ep.Snapshot.GPUs != nil {
		// Same rationale as Apps: scalar-only entries (DriverIndex
		// is a *int but the pointee is per-probe-fresh and never
		// mutated post-Observe), so copy the slice handle and let
		// individual GPUDetail values share their pointer aliases.
		cloned := make([]GPUDetail, len(ep.Snapshot.GPUs))
		copy(cloned, ep.Snapshot.GPUs)
		cp.Snapshot.GPUs = cloned
	}
	return &cp
}

// GetEndpointsForRequest returns snapshot copies of endpoints matching request.
// The returned Endpoint values are safe to read without holding any lock.
func (r *EndpointRegistry) GetEndpointsForRequest(req *Request) []*Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// If target host specified
	if req.TargetNode != "" && req.TargetNode != "*" {
		// Try exact URL match first (O(1))
		if ep, ok := r.endpoints[req.TargetNode]; ok {
			return []*Endpoint{copyEndpoint(ep)}
		}

		// Try indexed lookup by alias, name, or nodename (O(1) each)
		targetLower := strings.ToLower(req.TargetNode)
		for _, index := range []map[string]string{r.byAlias, r.byName, r.byNodeName} {
			if url, ok := index[targetLower]; ok {
				if ep, ok := r.endpoints[url]; ok {
					return []*Endpoint{copyEndpoint(ep)}
				}
			}
		}
		return nil
	}

	// Return all endpoints
	endpoints := make([]*Endpoint, 0, len(r.endpoints))
	for _, ep := range r.endpoints {
		endpoints = append(endpoints, copyEndpoint(ep))
	}

	return endpoints
}

// GetAllEndpoints returns snapshot copies of all registered endpoints.
// The returned Endpoint values are safe to read without holding any lock.
func (r *EndpointRegistry) GetAllEndpoints() []*Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()

	endpoints := make([]*Endpoint, 0, len(r.endpoints))
	for _, ep := range r.endpoints {
		endpoints = append(endpoints, copyEndpoint(ep))
	}
	return endpoints
}

// GetEndpointsByURLs returns snapshot copies of endpoints for given URLs.
func (r *EndpointRegistry) GetEndpointsByURLs(urls []string) []*Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()

	endpoints := make([]*Endpoint, 0, len(urls))
	for _, url := range urls {
		if ep, ok := r.endpoints[url]; ok {
			endpoints = append(endpoints, copyEndpoint(ep))
		}
	}
	return endpoints
}

// GetEndpointByURL returns a snapshot copy of the endpoint for given URL.
func (r *EndpointRegistry) GetEndpointByURL(url string) (*Endpoint, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if ep, ok := r.endpoints[url]; ok {
		return copyEndpoint(ep), nil
	}
	return nil, fmt.Errorf("endpoint not found: %s", url)
}

// Count returns number of registered endpoints
func (r *EndpointRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.endpoints)
}

// Observe applies a probe result to the endpoint's liveness state machine.
// Returns the resulting Transition (zero if state did not change). When
// probe.OK, the Connection snapshot is propagated into Endpoint.Snapshot
// (version, OS, RAM, GPU, running models, apps). On failure, the error
// is recorded in Snapshot.LastError.
//
// All mutation happens under the registry write lock; callers act on the
// returned Transition (log, fire reconnection callback) outside the lock.
func (r *EndpointRegistry) Observe(url string, probe ProbeResult) Transition {
	r.mu.Lock()
	defer r.mu.Unlock()

	ep, ok := r.endpoints[url]
	if !ok {
		utils.LogDebugf("[Registry] Observe: endpoint %s not in registry, skipping", url)
		return Transition{}
	}
	liv, ok := r.liveness[url]
	if !ok {
		liv = NewLiveness(livenessThreshold)
		r.liveness[url] = liv
	}

	t := liv.Observe(probe, utils.Now())
	ep.Status = liv.Status()

	if probe.Err != nil {
		ep.Snapshot.LastError = probe.Err.Error()
	} else {
		ep.Snapshot.LastError = ""
	}

	if probe.OK && probe.Snapshot != nil {
		r.applyConnectionSnapshot(ep, probe.Snapshot)
	}
	return t
}

// MarkDown forces a single endpoint to StatusDown immediately,
// bypassing the liveness hysteresis threshold. For use when a peer
// declares it is going away (graceful-shutdown goodbye notify) — the
// peer's own declaration is authoritative, so the N-consecutive-miss
// guard is unnecessary and would delay DOWN detection by at least one
// full health-poll interval. Returns the resulting Transition (zero
// if already DOWN or the URL is not registered).
func (r *EndpointRegistry) MarkDown(url string, reason error) Transition {
	r.mu.Lock()
	defer r.mu.Unlock()

	ep, ok := r.endpoints[url]
	if !ok {
		return Transition{}
	}
	liv, ok := r.liveness[url]
	if !ok {
		liv = NewLiveness(livenessThreshold)
		r.liveness[url] = liv
	}
	t := liv.ForceDown(utils.Now(), reason)
	ep.Status = liv.Status()
	if reason != nil {
		ep.Snapshot.LastError = reason.Error()
	}
	return t
}

// applyConnectionSnapshot propagates a successful probe's Connection into
// the endpoint's identity fields and typed Snapshot. Must be called with
// r.mu held for writing.
func (r *EndpointRegistry) applyConnectionSnapshot(ep *Endpoint, conn *Connection) {
	r.deindexEndpoint(ep)
	if conn.NodeName != "" {
		ep.Name = conn.NodeName
		ep.NodeName = conn.NodeName
	}
	r.indexEndpoint(ep)

	// Embedded HealthReport copies the 16 resource fields at once.
	// Registry-owned fields (Version string, LastError, CollectedAt)
	// are stamped separately — Connection does not populate them.
	// Version is stamped unconditionally to match pre-refactor
	// semantics: a nil conn.Version clears ep.Snapshot.Version to "".
	ep.Snapshot.HealthReport = conn.HealthReport
	ep.Snapshot.Version = ""
	if conn.Version != nil {
		ep.Snapshot.Version = conn.Version.String()
	}
	ep.Snapshot.CollectedAt = utils.Now()
}
