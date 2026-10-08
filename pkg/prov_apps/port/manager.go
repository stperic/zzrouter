package port

import (
	"fmt"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/net"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Range defines a port range for allocation.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Validate checks that the range is valid.
func (r Range) Validate() error {
	if r.Start < 1 || r.End > 65535 {
		return fmt.Errorf("port range must be between 1-65535, got %d-%d", r.Start, r.End)
	}
	if r.Start > r.End {
		return fmt.Errorf("start port (%d) must not exceed end port (%d)", r.Start, r.End)
	}
	return nil
}

// State represents the allocation state of a port.
type State string

const (
	StateFree     State = "free"
	StateAssigned State = "assigned"
	StateExternal State = "external" // used by non-zzRouter process
)

// Info tracks port allocation details.
type Info struct {
	Port       int
	State      State
	InstanceID string
	AssignedAt time.Time
}

// Pool manages port allocation for a single port range.
type Pool struct {
	portRange Range
	usedPorts map[int]bool
	portInfo  map[int]*Info
	mu        sync.RWMutex
}

// NewPool creates a new port pool. Returns an error if the range is invalid.
func NewPool(r Range) (*Pool, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &Pool{
		portRange: r,
		usedPorts: make(map[int]bool),
		portInfo:  make(map[int]*Info),
	}, nil
}

// Allocate allocates the next available port.
func (p *Pool) Allocate() (int, error) {
	return p.AllocateForInstance("")
}

// AllocateForInstance allocates a port and records the instance ID.
func (p *Pool) AllocateForInstance(instanceID string) (int, error) {
	// Scan listening ports once before acquiring the lock.
	listeningPorts := listeningPortSet()

	p.mu.Lock()
	defer p.mu.Unlock()

	port, ok := p.findFreePortLocked(listeningPorts, true)
	if !ok {
		return 0, p.exhaustedError()
	}
	p.usedPorts[port] = true
	p.portInfo[port] = &Info{
		Port:       port,
		State:      StateAssigned,
		InstanceID: instanceID,
		AssignedAt: utils.Now(),
	}
	return port, nil
}

// findFreePortLocked scans the range for the lowest free port. The caller
// must already hold an appropriate lock (write lock for callers that mutate
// portInfo, read lock for read-only callers). When recordExternal is true,
// listening ports get marked StateExternal in portInfo as a side effect — this
// requires the write lock and is used by AllocateForInstance.
func (p *Pool) findFreePortLocked(listeningPorts map[uint32]bool, recordExternal bool) (int, bool) {
	for port := p.portRange.Start; port <= p.portRange.End; port++ {
		if p.usedPorts[port] {
			continue
		}
		if listeningPorts[uint32(port)] {
			if recordExternal {
				p.portInfo[port] = &Info{Port: port, State: StateExternal}
			}
			continue
		}
		return port, true
	}
	return 0, false
}

func (p *Pool) exhaustedError() error {
	return fmt.Errorf("no available ports in range %d-%d", p.portRange.Start, p.portRange.End)
}

// isPortInUse checks a single port via syscall. Used only for AllocateSpecific
// where a single-port check is acceptable.
func (p *Pool) isPortInUse(port int) bool {
	return IsPortInUse(port)
}

// AllocateSpecific allocates a specific port if available and in range.
func (p *Pool) AllocateSpecific(port int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if port < p.portRange.Start || port > p.portRange.End {
		return fmt.Errorf("port %d outside range %d-%d", port, p.portRange.Start, p.portRange.End)
	}
	if p.usedPorts[port] {
		return fmt.Errorf("port %d already allocated", port)
	}
	if p.isPortInUse(port) {
		p.portInfo[port] = &Info{Port: port, State: StateExternal}
		return fmt.Errorf("port %d in use by external process", port)
	}

	p.usedPorts[port] = true
	p.portInfo[port] = &Info{
		Port:       port,
		State:      StateAssigned,
		AssignedAt: utils.Now(),
	}
	return nil
}

// AllocateWithFallback tries the requested port, falls back to auto-allocation.
func (p *Pool) AllocateWithFallback(requestedPort int) (int, error) {
	if requestedPort > 0 {
		if err := p.AllocateSpecific(requestedPort); err == nil {
			return requestedPort, nil
		}
	}
	return p.Allocate()
}

// Project returns the port that the next Allocate() call would hand out,
// without actually reserving it. Used by preview/dry-run paths that want to
// show the user what port will be assigned without affecting pool state.
// Returns an error if the pool is exhausted.
func (p *Pool) Project() (int, error) {
	listeningPorts := listeningPortSet()

	p.mu.RLock()
	defer p.mu.RUnlock()

	port, ok := p.findFreePortLocked(listeningPorts, false)
	if !ok {
		return 0, p.exhaustedError()
	}
	return port, nil
}

// Release returns a port to the pool. No-op if port was not allocated.
func (p *Pool) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.usedPorts[port] {
		return
	}
	delete(p.usedPorts, port)

	if info, ok := p.portInfo[port]; ok && info.State == StateAssigned {
		info.State = StateFree
		info.InstanceID = ""
		info.AssignedAt = time.Time{}
	}
}

// IsAllocated checks if a port is currently allocated.
func (p *Pool) IsAllocated(port int) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.usedPorts[port]
}

// ListAllocated returns all currently allocated ports.
func (p *Pool) ListAllocated() []int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ports := make([]int, 0, len(p.usedPorts))
	for port := range p.usedPorts {
		ports = append(ports, port)
	}
	return ports
}

// Stats returns port allocation statistics.
func (p *Pool) Stats() map[string]int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := map[string]int{
		"total":    p.portRange.End - p.portRange.Start + 1,
		"free":     0,
		"assigned": 0,
		"external": 0,
	}

	for port := p.portRange.Start; port <= p.portRange.End; port++ {
		if info, ok := p.portInfo[port]; ok {
			switch info.State {
			case StateFree:
				stats["free"]++
			case StateAssigned:
				stats["assigned"]++
			case StateExternal:
				stats["external"]++
			}
		} else {
			stats["free"]++
		}
	}
	return stats
}

// GetRange returns the port range for this pool.
func (p *Pool) GetRange() Range {
	return p.portRange
}

// --- AppPoolManager ---

// PoolStats represents statistics for a provider's port pool.
type PoolStats struct {
	Provider       string `json:"provider"`
	Mode           string `json:"mode"`
	RangeStart     int    `json:"range_start"`
	RangeEnd       int    `json:"range_end"`
	TotalPorts     int    `json:"total_ports"`
	AllocatedPorts int    `json:"allocated_ports"`
	AvailablePorts int    `json:"available_ports"`
	Ports          []int  `json:"ports"`
}

// AppPoolManager manages per-provider port pools built from provider config.
type AppPoolManager struct {
	pools map[string]*Pool
	modes map[string]string // provider → mode
	mu    sync.RWMutex
}

// NewAppPoolManager creates a port pool manager from apps config.
func NewAppPoolManager(appsConfig *config.AppsConfig) (*AppPoolManager, error) {
	m := &AppPoolManager{
		pools: make(map[string]*Pool),
		modes: make(map[string]string),
	}

	if appsConfig == nil {
		return m, nil
	}

	var poolErr error
	appsConfig.RangeApps(func(name string, svc config.ServiceConfig) bool {
		if !svc.IsEnabled() || svc.Runtime == nil {
			return true
		}
		m.modes[name] = svc.Mode
		pool, err := poolForService(svc)
		if err != nil {
			poolErr = fmt.Errorf("provider %q: %w", name, err)
			return false
		}
		if pool != nil {
			m.pools[name] = pool
		}
		return true
	})
	if poolErr != nil {
		return nil, poolErr
	}

	if err := m.validateNoOverlaps(); err != nil {
		return nil, err
	}
	return m, nil
}

// poolForService creates a port pool from a service config's runtime settings.
// Returns nil if the service doesn't need a port pool.
func poolForService(svc config.ServiceConfig) (*Pool, error) {
	if svc.Runtime == nil {
		return nil, nil
	}
	if svc.LaunchesProcess() && len(svc.Runtime.PortRange) == 2 {
		return NewPool(Range{
			Start: svc.Runtime.PortRange[0],
			End:   svc.Runtime.PortRange[1],
		})
	}
	if svc.HasEndpoint() {
		if basePort := svc.Runtime.GetBasePort(); basePort > 0 {
			return NewPool(Range{Start: basePort, End: basePort})
		}
	}
	return nil, nil
}

func (m *AppPoolManager) getPool(provider string) (*Pool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pool, ok := m.pools[provider]
	return pool, ok
}

// EnsurePool registers a port pool for a provider if one doesn't already exist.
// Called after a provider is installed/enabled mid-session so that LaunchInstance
// can allocate ports without requiring a server restart.
// Validates against overlapping port ranges before committing.
func (m *AppPoolManager) EnsurePool(name string, svc config.ServiceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.pools[name]; exists {
		return nil
	}

	pool, err := poolForService(svc)
	if err != nil {
		return fmt.Errorf("provider %q: %w", name, err)
	}
	if pool == nil {
		return nil
	}

	// Insert tentatively, validate, rollback on overlap
	m.pools[name] = pool
	m.modes[name] = svc.Mode
	if err := m.validateNoOverlaps(); err != nil {
		delete(m.pools, name)
		delete(m.modes, name)
		return err
	}

	return nil
}

// AllocateForProvider allocates a port from a provider's pool.
func (m *AppPoolManager) AllocateForProvider(provider string, requestedPort int) (int, error) {
	pool, ok := m.getPool(provider)
	if !ok {
		return 0, fmt.Errorf("no port pool for provider %q", provider)
	}
	return pool.AllocateWithFallback(requestedPort)
}

// ProjectForProvider returns the port that AllocateForProvider would hand out
// next without reserving it. Returns an error if the provider has no pool or
// the pool is exhausted.
func (m *AppPoolManager) ProjectForProvider(provider string) (int, error) {
	pool, ok := m.getPool(provider)
	if !ok {
		return 0, fmt.Errorf("no port pool for provider %q", provider)
	}
	return pool.Project()
}

// ReleaseForProvider releases a port back to a provider's pool.
func (m *AppPoolManager) ReleaseForProvider(provider string, port int) {
	if pool, ok := m.getPool(provider); ok {
		pool.Release(port)
	}
}

// IsAllocatedForProvider checks if a port is allocated in a provider's pool.
func (m *AppPoolManager) IsAllocatedForProvider(provider string, port int) bool {
	pool, ok := m.getPool(provider)
	if !ok {
		return false
	}
	return pool.IsAllocated(port)
}

// GetPoolStats returns statistics for all provider pools.
func (m *AppPoolManager) GetPoolStats() map[string]PoolStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := make(map[string]PoolStats)
	for name, pool := range m.pools {
		allocated := pool.ListAllocated()
		r := pool.GetRange()
		total := r.End - r.Start + 1

		stats[name] = PoolStats{
			Provider:       name,
			Mode:           m.modes[name],
			RangeStart:     r.Start,
			RangeEnd:       r.End,
			TotalPorts:     total,
			AllocatedPorts: len(allocated),
			AvailablePorts: total - len(allocated),
			Ports:          allocated,
		}
	}
	return stats
}

// GetProviderCapacity returns the maximum concurrent instances for a provider.
func (m *AppPoolManager) GetProviderCapacity(provider string) int {
	pool, ok := m.getPool(provider)
	if !ok {
		return 0
	}
	r := pool.GetRange()
	return r.End - r.Start + 1
}

// ListProviders returns all providers with port pools.
func (m *AppPoolManager) ListProviders() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	providers := make([]string, 0, len(m.pools))
	for name := range m.pools {
		providers = append(providers, name)
	}
	return providers
}

// ListAllAllocated returns all allocated ports across all providers.
func (m *AppPoolManager) ListAllAllocated() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []int
	for _, pool := range m.pools {
		all = append(all, pool.ListAllocated()...)
	}
	return all
}

func (m *AppPoolManager) validateNoOverlaps() error {
	type entry struct {
		name   string
		range_ Range
	}

	entries := make([]entry, 0, len(m.pools))
	for name, pool := range m.pools {
		entries = append(entries, entry{name, pool.GetRange()})
	}

	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			a, b := entries[i], entries[j]
			if a.range_.Start <= b.range_.End && b.range_.Start <= a.range_.End {
				return fmt.Errorf("%w: %q [%d-%d] overlaps %q [%d-%d]",
					ErrPortConflict,
					a.name, a.range_.Start, a.range_.End,
					b.name, b.range_.Start, b.range_.End)
			}
		}
	}
	return nil
}

// listeningPortSet returns the set of all TCP ports currently in LISTEN or
// ESTABLISHED state. It makes a single syscall, so callers that need to check
// multiple ports should call this once and probe the returned map.
func listeningPortSet() map[uint32]bool {
	connections, err := net.Connections("tcp")
	if err != nil {
		return nil
	}
	ports := make(map[uint32]bool, len(connections))
	for _, conn := range connections {
		if conn.Status == "LISTEN" || conn.Status == "ESTABLISHED" {
			ports[conn.Laddr.Port] = true
		}
	}
	return ports
}

// IsPortInUse checks if a single port is currently in use on the system.
// For checking multiple ports, prefer listeningPortSet to avoid repeated syscalls.
func IsPortInUse(port int) bool {
	return listeningPortSet()[uint32(port)]
}
