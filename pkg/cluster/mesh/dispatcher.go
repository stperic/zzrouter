package mesh

import (
	"context"
	"fmt"
	"sync"
)

// Dispatcher handles request dispatch with different strategies
type Dispatcher struct {
	connector       *Connector
	circuitBreakers *CircuitBreakerManager
	localHandler    LocalHandler // For in-process local dispatch
	localNodeURL    string
	mu              sync.RWMutex
	strategies      map[DispatchStrategy]DispatchHandler
}

// DispatcherConfig holds dispatcher configuration
type DispatcherConfig struct {
	Connector       *Connector
	CircuitBreakers *CircuitBreakerManager
	LocalHandler    LocalHandler
	LocalNodeURL    string
}

// DispatchHandler is the interface for dispatch strategies
type DispatchHandler interface {
	Dispatch(ctx context.Context, endpoints []*Endpoint, req *Request) (*Response, error)
}

// NewDispatcher creates a new dispatcher
func NewDispatcher(config *DispatcherConfig) *Dispatcher {
	d := &Dispatcher{
		connector:       config.Connector,
		circuitBreakers: config.CircuitBreakers,
		localHandler:    config.LocalHandler,
		localNodeURL:    config.LocalNodeURL,
		strategies:      make(map[DispatchStrategy]DispatchHandler),
	}

	// Register built-in strategies
	d.strategies[StrategyUnicast] = NewUnicastHandler(d)
	d.strategies[StrategyBroadcast] = NewBroadcastHandler(d)
	d.strategies[StrategyRoundRobin] = NewRoundRobinHandler(d)
	d.strategies[StrategyFailover] = NewFailoverHandler(d)
	// Streaming falls back to unicast: the cluster dispatcher picks a single node,
	// and the actual SSE/streaming is handled end-to-end by the HTTP proxy layer above.
	d.strategies[StrategyStreaming] = NewUnicastHandler(d)

	return d
}

// Dispatch dispatches request using appropriate strategy
func (d *Dispatcher) Dispatch(ctx context.Context, strategy DispatchStrategy, endpoints []*Endpoint, req *Request) (*Response, error) {
	d.mu.RLock()
	handler, ok := d.strategies[strategy]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown dispatch strategy: %s", strategy.String())
	}

	return handler.Dispatch(ctx, endpoints, req)
}

// RegisterStrategy allows custom strategies to be registered
func (d *Dispatcher) RegisterStrategy(strategy DispatchStrategy, handler DispatchHandler) {
	d.mu.Lock()
	d.strategies[strategy] = handler
	d.mu.Unlock()
}
