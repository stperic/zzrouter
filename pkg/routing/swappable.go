package routing

import (
	"context"
	"sync/atomic"
)

// Swappable is a Router that delegates to an inner Router which can be
// atomically replaced at runtime. All services that capture a Router at boot
// continue to see the current strategy after a swap — no re-wiring needed.
//
// Use it to follow cluster mode transitions (standalone ↔ worker) without
// tearing down and rebuilding the server graph.
type Swappable struct {
	inner atomic.Pointer[Router]
}

// NewSwappable wraps initial and returns a Router whose backing implementation
// can be replaced via Swap. Panics if initial is nil — a Swappable with no
// backing router would panic on the first call anyway, so fail loudly at
// construction instead.
func NewSwappable(initial Router) *Swappable {
	if initial == nil {
		panic("routing.NewSwappable: initial Router must not be nil")
	}
	s := &Swappable{}
	s.inner.Store(&initial)
	return s
}

// Swap atomically replaces the backing Router and returns the previous one.
// Panics if next is nil.
func (s *Swappable) Swap(next Router) Router {
	if next == nil {
		panic("routing.Swappable.Swap: next Router must not be nil")
	}
	prev := s.inner.Swap(&next)
	return *prev
}

// Current returns the Router currently serving requests. Exposed mainly for
// tests and diagnostics; handlers should call Router methods directly.
func (s *Swappable) Current() Router {
	return *s.inner.Load()
}

func (s *Swappable) Unicast(ctx context.Context, host, path, method string, body []byte) (*Response, error) {
	return s.Current().Unicast(ctx, host, path, method, body)
}

func (s *Swappable) Broadcast(ctx context.Context, path, method string, body []byte) (*Response, error) {
	return s.Current().Broadcast(ctx, path, method, body)
}

func (s *Swappable) Route(ctx context.Context, req *Request) (*Response, error) {
	return s.Current().Route(ctx, req)
}

func (s *Swappable) MultiRoute(ctx context.Context, path, method string, hostBodies map[string][]byte) (*Response, error) {
	return s.Current().MultiRoute(ctx, path, method, hostBodies)
}
