package fallback

// CooldownSet groups the per-deployment and per-provider cooldown managers
// behind a single Start/Stop lifecycle. Individual managers are exposed via
// Deployments() and Providers() for consumers that need them directly.
type CooldownSet struct {
	deployments *CooldownManager
	providers   *CooldownManager
}

// NewCooldownSet creates the pair of cooldown managers.
func NewCooldownSet() *CooldownSet {
	return &CooldownSet{
		deployments: NewCooldownManager(),
		providers:   NewCooldownManager(),
	}
}

// Deployments returns the per-deployment cooldown manager.
func (cs *CooldownSet) Deployments() *CooldownManager { return cs.deployments }

// Providers returns the per-provider cooldown manager.
func (cs *CooldownSet) Providers() *CooldownManager { return cs.providers }

// Start launches cleanup goroutines for both managers.
func (cs *CooldownSet) Start() {
	if cs == nil {
		return
	}
	cs.deployments.Start()
	cs.providers.Start()
}

// Stop terminates cleanup goroutines for both managers.
func (cs *CooldownSet) Stop() {
	if cs == nil {
		return
	}
	cs.deployments.Stop()
	cs.providers.Stop()
}
