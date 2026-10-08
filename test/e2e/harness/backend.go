package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// backendFor maps a Backend tag to its registered implementation. The
// registry is consulted FIRST so a sub-package's init-time
// RegisterBackend(BackendInproc, ...) wins over the no-op fallback.
// An empty tag falls through to the external-URL backend (caller
// already has the addresses running, just dial).
//
// An explicit BackendInproc / BackendDocker / BackendSSH that is not
// registered errors with a hint to import the sub-package — silent
// fallback to externalBackend would mask missing imports and make
// tests appear to pass while dialing whatever happened to be on the
// expected port.
func backendFor(tag Backend) (Provisioner, error) {
	if tag != "" {
		backendsMu.Lock()
		ctor, ok := backends[tag]
		backendsMu.Unlock()
		if ok {
			return ctor(), nil
		}
	}
	if tag == "" {
		return externalBackend{}, nil
	}
	return nil, fmt.Errorf("backend %q not registered: blank-import test/e2e/harness/%s or pass an override to New", tag, strings.ToLower(string(tag)))
}

var (
	backendsMu sync.Mutex
	backends   = map[Backend]func() Provisioner{}
)

// RegisterBackend installs a Provisioner constructor under the given
// tag. Sub-packages call this from init() so a test profile that
// names the backend gets it without an explicit import in the test
// file. Panics on double-registration — that's a programmer error.
func RegisterBackend(tag Backend, ctor func() Provisioner) {
	backendsMu.Lock()
	defer backendsMu.Unlock()
	if _, exists := backends[tag]; exists {
		panic(fmt.Sprintf("harness: backend %q already registered", tag))
	}
	backends[tag] = ctor
}

// externalBackend is the simplest possible Backend: it does not start
// or stop anything. Addresses must already be reachable (set via the
// cluster topology in the Config). Used as the BackendInproc default
// until the in-process subprocess wiring is available, and as the
// canonical "I'm dialing a real running cluster" backend.
//
// Cluster keys + admin keys come from env (validated at config Load).
type externalBackend struct{}

func (externalBackend) Name() string { return "external" }

func (externalBackend) Provision(ctx context.Context, cfg *Config) (*Node, []*Node, error) {
	if cfg.Cluster.Coordinator.Address == "" {
		return nil, nil, errors.New("cluster.coordinator.address required for external backend")
	}
	keys := NodeKeys{
		Admin:   os.Getenv("ZZROUTER_ADMIN_API_KEY"),
		API:     os.Getenv("ZZROUTER_API_KEY"),
		Cluster: os.Getenv("ZZROUTER_CLUSTER_NETWORK_KEY"),
	}
	coord, err := nodeFromSpec(cfg.Cluster.Coordinator, keys)
	if err != nil {
		return nil, nil, fmt.Errorf("coordinator: %w", err)
	}
	workers := make([]*Node, 0, len(cfg.Cluster.Workers))
	for _, w := range cfg.Cluster.Workers {
		if !w.IsEnabled() {
			continue
		}
		n, err := nodeFromSpec(w, keys)
		if err != nil {
			return nil, nil, fmt.Errorf("worker %s: %w", w.Name, err)
		}
		workers = append(workers, n)
	}
	return coord, workers, nil
}

func (externalBackend) Teardown(ctx context.Context) error { return nil }

func nodeFromSpec(s NodeSpec, keys NodeKeys) (*Node, error) {
	port := s.APIPort
	if port == 0 {
		port = 9090
	}
	host := s.Address
	if host == "" {
		host = "127.0.0.1"
	}
	return NewNode(
		s.Name,
		fmt.Sprintf("http://%s:%d", host, port),
		s.Role,
		s.Tags,
		keys,
	)
}
