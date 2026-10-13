package port

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustNewPool creates a Pool for testing, failing the test on invalid range.
func mustNewPool(t *testing.T, r Range) *Pool {
	t.Helper()
	p, err := NewPool(r)
	require.NoError(t, err)
	return p
}

func TestRange_Validate(t *testing.T) {
	tests := []struct {
		name    string
		r       Range
		wantErr bool
	}{
		{"valid", Range{8000, 8010}, false},
		{"single port", Range{8000, 8000}, false},
		{"start > end", Range{8010, 8000}, true},
		{"below minimum", Range{0, 100}, true},
		{"above maximum", Range{60000, 70000}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.r.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNewPool_InvalidRange(t *testing.T) {
	_, err := NewPool(Range{8010, 8000})
	assert.Error(t, err)

	_, err = NewPool(Range{0, 100})
	assert.Error(t, err)
}

func TestPool_Allocate(t *testing.T) {
	p := mustNewPool(t, Range{9000, 9002})

	port1, err := p.Allocate()
	require.NoError(t, err)
	assert.Equal(t, 9000, port1)

	port2, err := p.Allocate()
	require.NoError(t, err)
	assert.Equal(t, 9001, port2)

	port3, err := p.Allocate()
	require.NoError(t, err)
	assert.Equal(t, 9002, port3)

	_, err = p.Allocate()
	assert.Error(t, err)
}

func TestPool_AllocateSpecific(t *testing.T) {
	p := mustNewPool(t, Range{9000, 9010})

	require.NoError(t, p.AllocateSpecific(9005))
	assert.True(t, p.IsAllocated(9005))

	err := p.AllocateSpecific(9005)
	assert.Error(t, err) // already allocated

	err = p.AllocateSpecific(8000)
	assert.Error(t, err) // out of range
}

func TestPool_AllocateWithFallback(t *testing.T) {
	p := mustNewPool(t, Range{9000, 9010})

	// Specific port available
	port, err := p.AllocateWithFallback(9005)
	require.NoError(t, err)
	assert.Equal(t, 9005, port)

	// Specific port already taken, falls back
	port, err = p.AllocateWithFallback(9005)
	require.NoError(t, err)
	assert.Equal(t, 9000, port) // first available

	// No specific port requested
	port, err = p.AllocateWithFallback(0)
	require.NoError(t, err)
	assert.Equal(t, 9001, port)
}

func TestPool_Release(t *testing.T) {
	p := mustNewPool(t, Range{9000, 9000})

	port, err := p.Allocate()
	require.NoError(t, err)
	assert.Equal(t, 9000, port)

	_, err = p.Allocate()
	assert.Error(t, err) // exhausted

	p.Release(9000)

	port, err = p.Allocate()
	require.NoError(t, err)
	assert.Equal(t, 9000, port)
}

func TestPool_Release_Unallocated(t *testing.T) {
	p := mustNewPool(t, Range{9000, 9010})

	// Releasing a port that was never allocated should be a no-op.
	p.Release(9005)
	assert.False(t, p.IsAllocated(9005))
}

func TestPool_ListAllocated(t *testing.T) {
	p := mustNewPool(t, Range{9000, 9010})

	_, _ = p.AllocateForInstance("inst-1")
	_, _ = p.AllocateForInstance("inst-2")

	allocated := p.ListAllocated()
	assert.Len(t, allocated, 2)
}

func TestPool_Stats(t *testing.T) {
	p := mustNewPool(t, Range{9000, 9004})

	_, _ = p.AllocateForInstance("inst-1")
	_, _ = p.AllocateForInstance("inst-2")

	stats := p.Stats()
	assert.Equal(t, 5, stats["total"])
	assert.Equal(t, 2, stats["assigned"])
}

func TestPool_Concurrent(t *testing.T) {
	// Use a large range to avoid conflicts with ports in use on the test machine.
	p := mustNewPool(t, Range{39000, 39199})

	const goroutines = 100
	var wg sync.WaitGroup
	allocated := make(chan int, goroutines)

	for range goroutines {
		wg.Go(func() {
			port, err := p.Allocate()
			if err == nil {
				allocated <- port
			}
		})
	}

	wg.Wait()
	close(allocated)

	// All allocated ports must be unique; some may fail if OS ports are in use.
	ports := make(map[int]bool)
	for port := range allocated {
		assert.False(t, ports[port], "port %d allocated twice", port)
		ports[port] = true
	}
	assert.GreaterOrEqual(t, len(ports), goroutines-5, "most ports should be allocated")
}

func TestAppPoolManager_ValidateNoOverlaps(t *testing.T) {
	m := &AppPoolManager{
		pools: map[string]*Pool{
			"vllm":      mustNewPool(t, Range{8000, 8010}),
			"llama.cpp": mustNewPool(t, Range{8005, 8015}), // overlaps with vllm
		},
		modes: map[string]string{
			"vllm":      "on-demand",
			"llama.cpp": "on-demand",
		},
	}

	err := m.validateNoOverlaps()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPortConflict)
}

func TestAppPoolManager_NoOverlap(t *testing.T) {
	m := &AppPoolManager{
		pools: map[string]*Pool{
			"vllm":      mustNewPool(t, Range{8000, 8010}),
			"llama.cpp": mustNewPool(t, Range{8020, 8030}),
		},
		modes: map[string]string{
			"vllm":      "on-demand",
			"llama.cpp": "on-demand",
		},
	}

	assert.NoError(t, m.validateNoOverlaps())
}

func TestAppPoolManager_AllocateAndRelease(t *testing.T) {
	m := &AppPoolManager{
		pools: map[string]*Pool{
			"vllm": mustNewPool(t, Range{9000, 9005}),
		},
		modes: map[string]string{"vllm": "on-demand"},
	}

	port, err := m.AllocateForProvider("vllm", 0)
	require.NoError(t, err)
	assert.Equal(t, 9000, port)

	assert.True(t, m.IsAllocatedForProvider("vllm", 9000))

	m.ReleaseForProvider("vllm", 9000)
	assert.False(t, m.IsAllocatedForProvider("vllm", 9000))
}

func TestPool_ProjectDoesNotReserve(t *testing.T) {
	pool, err := NewPool(Range{Start: 9100, End: 9105})
	require.NoError(t, err)

	first, err := pool.Project()
	require.NoError(t, err)

	// Calling Project again returns the same port — nothing was reserved.
	second, err := pool.Project()
	require.NoError(t, err)
	assert.Equal(t, first, second, "Project must not mutate pool state")

	// And the projected port is what Allocate then hands out.
	allocated, err := pool.Allocate()
	require.NoError(t, err)
	assert.Equal(t, first, allocated)

	// After allocation, Project advances to the next free port.
	next, err := pool.Project()
	require.NoError(t, err)
	assert.Equal(t, allocated+1, next)
}

func TestAppPoolManager_ProjectForProvider(t *testing.T) {
	m := &AppPoolManager{
		pools: map[string]*Pool{
			"vllm": mustNewPool(t, Range{9200, 9205}),
		},
		modes: map[string]string{"vllm": "on-demand"},
	}
	port, err := m.ProjectForProvider("vllm")
	require.NoError(t, err)
	assert.Equal(t, 9200, port)
	assert.False(t, m.IsAllocatedForProvider("vllm", 9200), "Project must not reserve")

	_, err = m.ProjectForProvider("missing")
	assert.Error(t, err)
}

func TestAppPoolManager_UnknownProvider(t *testing.T) {
	m := &AppPoolManager{
		pools: make(map[string]*Pool),
		modes: make(map[string]string),
	}

	_, err := m.AllocateForProvider("nonexistent", 0)
	assert.Error(t, err)

	assert.False(t, m.IsAllocatedForProvider("nonexistent", 9000))
}

func TestAppPoolManager_GetPoolStats(t *testing.T) {
	m := &AppPoolManager{
		pools: map[string]*Pool{
			"vllm":      mustNewPool(t, Range{9000, 9004}),
			"llama.cpp": mustNewPool(t, Range{9010, 9014}),
		},
		modes: map[string]string{
			"vllm":      "on-demand",
			"llama.cpp": "on-demand",
		},
	}

	_, _ = m.AllocateForProvider("vllm", 0)
	_, _ = m.AllocateForProvider("vllm", 0)

	stats := m.GetPoolStats()
	assert.Len(t, stats, 2)
	assert.Equal(t, 2, stats["vllm"].AllocatedPorts)
	assert.Equal(t, 3, stats["vllm"].AvailablePorts)
	assert.Equal(t, 0, stats["llama.cpp"].AllocatedPorts)
}

func TestAppPoolManager_GetProviderCapacity(t *testing.T) {
	m := &AppPoolManager{
		pools: map[string]*Pool{
			"vllm": mustNewPool(t, Range{9000, 9004}),
		},
		modes: map[string]string{"vllm": "on-demand"},
	}

	assert.Equal(t, 5, m.GetProviderCapacity("vllm"))
	assert.Equal(t, 0, m.GetProviderCapacity("unknown"))
}

func TestAppPoolManager_ListProviders(t *testing.T) {
	m := &AppPoolManager{
		pools: map[string]*Pool{
			"vllm":      mustNewPool(t, Range{9000, 9004}),
			"llama.cpp": mustNewPool(t, Range{9010, 9014}),
		},
		modes: map[string]string{
			"vllm":      "on-demand",
			"llama.cpp": "on-demand",
		},
	}

	providers := m.ListProviders()
	assert.Len(t, providers, 2)
	assert.Contains(t, providers, "vllm")
	assert.Contains(t, providers, "llama.cpp")
}
