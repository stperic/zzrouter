package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// Cluster is the harness-side handle on a running zzrouter cluster
// under test. Construct with New(cfg); call Provision before use and
// Teardown on cleanup. All methods are safe for concurrent use after
// Provision returns.
//
// Polymorphism is scoped to provisioning via the Backend interface.
// The Cluster surface above the backend is a single concrete struct —
// per the "small interfaces at boundaries, structs in the middle"
// design rule.
type Cluster struct {
	cfg         *Config
	backend     Provisioner
	coord       *Node
	workers     []*Node
	mu          sync.Mutex
	provisioned bool
}

// Provisioner brings up + tears down the cluster topology. Each
// concrete implementation owns its own provisioning details
// (subprocess vs SSH vs in-process) but exposes the same shape so the
// rest of the harness is backend-agnostic. The Backend tag in Config
// selects which Provisioner to use; sub-packages register
// implementations via RegisterBackend.
type Provisioner interface {
	// Provision starts every node declared in the topology and returns
	// the resolved Node handles (with BaseURL filled in). Idempotent.
	Provision(ctx context.Context, cfg *Config) (coord *Node, workers []*Node, err error)

	// Teardown stops every node started by Provision. Best-effort —
	// errors are logged by the caller.
	Teardown(ctx context.Context) error

	// Name returns a short identifier for diagnostics + log context.
	Name() string
}

// New constructs a Cluster from a validated Config. The Provisioner is
// looked up by cfg.Backend; pass a custom Provisioner to override
// (e.g. for docker-compose or a future Antithesis driver).
func New(cfg *Config, override ...Provisioner) (*Cluster, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	var p Provisioner
	if len(override) > 0 && override[0] != nil {
		p = override[0]
	} else {
		var err error
		p, err = backendFor(cfg.Backend)
		if err != nil {
			return nil, err
		}
	}
	return &Cluster{cfg: cfg, backend: p}, nil
}

// ErrAlreadyProvisioned signals a double-Provision call — typically a
// lifecycle bug in test code that should be loud, not silent.
var ErrAlreadyProvisioned = errors.New("cluster already provisioned")

// Provision brings up the cluster. Errors with ErrAlreadyProvisioned
// on a second call so test lifecycle bugs surface immediately.
func (c *Cluster) Provision(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provisioned {
		return ErrAlreadyProvisioned
	}
	coord, workers, err := c.backend.Provision(ctx, c.cfg)
	if err != nil {
		return fmt.Errorf("backend %s: provision: %w", c.backend.Name(), err)
	}
	c.coord = coord
	c.workers = workers
	c.provisioned = true
	return nil
}

// Teardown stops the cluster. Idempotent.
func (c *Cluster) Teardown(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.provisioned {
		return nil
	}
	c.provisioned = false
	return c.backend.Teardown(ctx)
}

// Coordinator returns the coordinator node handle. Returns nil before
// Provision has succeeded.
func (c *Cluster) Coordinator() *Node {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.coord
}

// Workers returns the worker node handles. Returns nil before
// Provision; safe to call on a worker-less topology.
func (c *Cluster) Workers() []*Node {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Node, len(c.workers))
	copy(out, c.workers)
	return out
}

// Backend returns the underlying Provisioner. Tests use this to
// reach backend-specific accessors (e.g. ssh.Backend.LogFetcher)
// without bypassing the registry — the harness no longer forces
// the live-test "construct fresh + pass as override" workaround.
func (c *Cluster) Backend() Provisioner {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backend
}

// NodeByName resolves a node handle by its declared name. Errors when
// the name is unknown — callers should not silently fall through.
func (c *Cluster) NodeByName(name string) (*Node, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.coord != nil && c.coord.name == name {
		return c.coord, nil
	}
	for _, w := range c.workers {
		if w.name == name {
			return w, nil
		}
	}
	return nil, fmt.Errorf("no node named %q", name)
}

// Node is the harness-side handle on a single running zzrouter node.
// Concrete struct (not interface) — the only polymorphism is at the
// backend boundary. Public accessors are read-only after Provision.
type Node struct {
	name       string
	baseURL    string
	role       Role
	tags       []string
	adminKey   string
	apiKey     string // ZZROUTER_API_KEY — read-only inference tier
	clusterKey string
	unrecorded bool
}

// NodeOption adjusts a Node at construction. Variadic so the five
// positional arguments do not grow for something that applies to one
// node in the suites.
type NodeOption func(*Node)

// Unrecorded keeps a node's traffic out of the API coverage ledger.
// For test doubles only.
//
// A meta-test that stands up a deliberately broken handler still
// drives it through the harness client, and the recorder cannot tell
// a stub from the server: docs/api_examples.md published a fake's
// {"ok":true} as the response shape of GET /zzrouter/v1/keys because
// of exactly that. The ledger is evidence about the system under
// test, so a double has to say it is one.
func Unrecorded() NodeOption {
	return func(n *Node) { n.unrecorded = true }
}

// NodeKeys bundles the three static credentials a Node needs. Passed
// to NewNode so the constructor signature doesn't grow per-credential.
type NodeKeys struct {
	Admin   string
	API     string
	Cluster string
}

// NewNode constructs a Node handle. Returns nil + error when name or
// baseURL are empty — those are programmer errors callers should
// surface rather than silently propagate.
func NewNode(name, baseURL string, role Role, tags []string, keys NodeKeys, opts ...NodeOption) (*Node, error) {
	if name == "" {
		return nil, fmt.Errorf("node name required")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("node baseURL required")
	}
	tagsCopy := make([]string, len(tags))
	copy(tagsCopy, tags)
	n := &Node{
		name:       name,
		baseURL:    baseURL,
		role:       role,
		tags:       tagsCopy,
		adminKey:   keys.Admin,
		apiKey:     keys.API,
		clusterKey: keys.Cluster,
	}
	for _, opt := range opts {
		opt(n)
	}
	return n, nil
}

func (n *Node) Name() string       { return n.name }
func (n *Node) BaseURL() string    { return n.baseURL }
func (n *Node) Role() Role         { return n.role }
func (n *Node) AdminKey() string   { return n.adminKey }
func (n *Node) APIKey() string     { return n.apiKey }
func (n *Node) ClusterKey() string { return n.clusterKey }

// Tags returns a defensive copy.
func (n *Node) Tags() []string {
	out := make([]string, len(n.tags))
	copy(out, n.tags)
	return out
}

// HasTag is the common case wrapper around Tags.
func (n *Node) HasTag(tag string) bool {
	for _, t := range n.tags {
		if t == tag {
			return true
		}
	}
	return false
}

// HTTPClient returns the shared *http.Client for ALL requests against
// the node, streaming included: the client carries no timeout, so SSE
// and chunked reads stay open and callers set deadlines via context.
//
// Every harness request path resolves its client here, which is what
// makes this the one place API coverage has to be recorded — see
// coverage.go. A helper that builds its own client silently drops out
// of the ledger, so route requests through this one.
//
// The exception is a node built with Unrecorded: a test double is not
// the system under test and its answers are not evidence about it.
func (n *Node) HTTPClient() *http.Client {
	if n.unrecorded {
		return plainClient
	}
	return coverageClient
}
