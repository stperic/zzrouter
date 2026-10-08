package fallback

import "time"

// DeploymentStrategy selects and orders deployment candidates for a request.
// The input slice has already been filtered (health, cooldowns, tags).
// Implementations must not modify the input slice.
type DeploymentStrategy interface {
	// Select returns candidates ordered by preference for this request.
	Select(candidates []Candidate) []Candidate

	// Name returns the strategy identifier (e.g., "priority", "least-load").
	Name() string
}

// Candidate carries deployment info plus runtime metadata needed for selection.
type Candidate struct {
	Name       string
	Model      string
	App        string
	Node       string
	Priority   int
	Timeout    time.Duration
	OnDemand   bool
	Tags       []string
	MaxRetries int
}

// StrategyRegistry maps strategy names to implementations.
type StrategyRegistry struct {
	strategies map[string]DeploymentStrategy
}

// NewStrategyRegistry creates an empty strategy registry.
func NewStrategyRegistry() *StrategyRegistry {
	return &StrategyRegistry{
		strategies: make(map[string]DeploymentStrategy),
	}
}

// Register adds a strategy to the registry, keyed by its Name().
func (r *StrategyRegistry) Register(s DeploymentStrategy) {
	r.strategies[s.Name()] = s
}

// Get returns the strategy for the given name, or nil if not found.
func (r *StrategyRegistry) Get(name string) DeploymentStrategy {
	return r.strategies[name]
}
