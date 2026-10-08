// Package inproc provides the in-process Backend for the e2e test
// harness. Each node is a real *server.Server wrapped in an
// httptest.NewServer, so requests exercise the full middleware chain
// (auth, RFC 9457 envelopes, X-Request-ID) — not a mock.
//
// Importing this package as a blank import registers the Backend
// under harness.BackendInproc:
//
//	import _ "github.com/stperic/zzrouter/test/e2e/harness/inproc"
//
// Cluster pairing is NOT performed: the coordinator and worker run as
// independent servers without a real mTLS handshake. That covers
// every Ring 1 contract test (per-route auth + envelope shape) but
// NOT cross-node cluster-routing tests, which need the SSH or docker
// backend.
package inproc

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"

	"github.com/stperic/zzrouter/internal/server"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/test/e2e/harness"
)

func init() {
	harness.RegisterBackend(harness.BackendInproc, func() harness.Provisioner {
		return &Backend{}
	})
}

// Backend implements harness.Provisioner using server.BuildTestServer
// + httptest.NewServer per node.
type Backend struct {
	mu       sync.Mutex
	cleanups []func() // ordered teardown — workers first, coord last
}

// Name reports the registry tag for diagnostics.
func (b *Backend) Name() string { return "inproc" }

// Provision starts the coordinator + every enabled worker. Each node
// gets its own real *Server with a private temp ZZROUTER_TEST_HOME
// (BuildTestServer handles isolation).
func (b *Backend) Provision(ctx context.Context, cfg *harness.Config) (*harness.Node, []*harness.Node, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cleanups) > 0 {
		return nil, nil, errors.New("inproc backend already provisioned")
	}

	keys := harness.NodeKeys{
		Admin:   server.TestAdminKey,
		API:     server.TestUserKey,
		Cluster: server.TestClusterKey,
	}

	//nolint:contextcheck // booting a node is not request-scoped; teardown is driven by b.cleanups
	coordNode, coordCleanup, err := b.bootNode(cfg.Cluster.Coordinator.Name, harness.RoleCoordinator,
		cfg.Cluster.Coordinator.Tags, keys)
	if err != nil {
		return nil, nil, fmt.Errorf("coordinator: %w", err)
	}
	b.cleanups = append(b.cleanups, coordCleanup)

	workers := make([]*harness.Node, 0, len(cfg.Cluster.Workers))
	for _, w := range cfg.Cluster.Workers {
		if !w.IsEnabled() {
			continue
		}
		//nolint:contextcheck // booting a node is not request-scoped; teardown is driven by b.cleanups
		wn, wc, err := b.bootNode(w.Name, harness.RoleWorker, w.Tags, keys)
		if err != nil {
			// Tear down anything already started — don't leak servers.
			b.teardownLocked(ctx)
			return nil, nil, fmt.Errorf("worker %s: %w", w.Name, err)
		}
		b.cleanups = append(b.cleanups, wc)
		workers = append(workers, wn)
	}
	return coordNode, workers, nil
}

// bootNode constructs a real Server in the given role, wraps its gin
// engine in httptest.NewServer, and returns a harness.Node pointing at
// the live URL plus a cleanup func.
func (b *Backend) bootNode(name string, role harness.Role, tags []string, keys harness.NodeKeys) (*harness.Node, func(), error) {
	mode := pkgConfig.ClusterModeDisabled
	switch role {
	case harness.RoleCoordinator:
		mode = pkgConfig.ClusterModeCoordinator
	case harness.RoleWorker:
		mode = pkgConfig.ClusterModeWorker
	}
	srv, cleanup, err := server.BuildTestServer(server.TestNodeConfig{
		AdminKey:    keys.Admin,
		UserKey:     keys.API,
		ClusterKey:  keys.Cluster,
		ClusterMode: mode,
		NodeName:    name,
	})
	if err != nil {
		return nil, nil, err
	}
	// *Server implements http.Handler via ServeHTTP, so we can hand it
	// directly to httptest without exposing the gin engine.
	httpSrv := httptest.NewServer(srv)
	combined := func() {
		httpSrv.Close()
		cleanup()
	}
	node, err := harness.NewNode(name, httpSrv.URL, role, tags, keys)
	if err != nil {
		combined()
		return nil, nil, err
	}
	return node, combined, nil
}

// Teardown runs every cleanup recorded during Provision in reverse
// order (workers first, coord last) so cluster-routing teardown
// observes the same shutdown sequence a real cluster would.
func (b *Backend) Teardown(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.teardownLocked(ctx)
	return nil
}

func (b *Backend) teardownLocked(_ context.Context) {
	for i := len(b.cleanups) - 1; i >= 0; i-- {
		b.cleanups[i]()
	}
	b.cleanups = nil
}
