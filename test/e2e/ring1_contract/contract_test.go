// Package ring1_contract is the e2e harness "Ring 1" contract gate:
// every registered route is exercised against the running server with
// the four canonical auth cases (none / wrong-tier / correct-tier /
// invalid-body) plus the RFC 9457 envelope shape on errors.
//
// The driver is intentionally tiny. It reads the route catalog from
// the running server (no AST parse), partitions read-only vs mutating
// routes, parallelizes the read-only batch, and serializes the
// per-surface mutation batch with namespace-prefixed entities so
// concurrent writes don't collide.
//
// Coverage philosophy: prove the auth + envelope + headers contract
// for every route. DON'T prove business logic — that lives in Ring 2+
// where real provider/model fixtures are wired.
package ring1contract

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/internal/server"
	"github.com/stperic/zzrouter/test/e2e/harness"
	"github.com/stperic/zzrouter/test/e2e/harness/assert"
	_ "github.com/stperic/zzrouter/test/e2e/harness/inproc" // registers BackendInproc
	"go.uber.org/goleak"
)

// TestMain enforces the goleak invariant: any goroutine left running
// past the suite's last cleanup is a regression.
//
// The ignore set is the budget of known-unstopped subsystems. Each
// entry is a pin: when a leak shows up in CI that ISN'T listed here,
// the test fails. The right move when an entry needs to be added is
// to extend cleanupTestNode (internal/server/integration_test_helpers.go)
// to call the subsystem's Stop method, not to grow this list.
//
// Currently-pinned leaks (tracked for cleanupTestNode hardening):
//   - cluster.mesh.HealthMonitor: monitorLoop runs from the cluster
//     subsystem; cleanupTestNode doesn't reach it.
//   - cluster.node.PairingStore: gcLoop runs from cluster init.
//   - jobs.Registry.janitorLoop: WAS pinned; cleanupTestNode now calls
//     s.jobs.Stop(). Listed here as a tripwire — if it reappears, the
//     Stop call regressed.
func TestMain(m *testing.M) {
	code := m.Run()
	teardownSuite()
	// Emit the observed-API ledger before the leak check, so a
	// leak-failed run still reports what it exercised.
	if err := harness.WriteCoverageLedger("ring1_contract"); err != nil {
		fmt.Fprintf(os.Stderr, "coverage ledger: %v\n", err)
		code = 1
	}
	if code == 0 {
		if err := goleak.Find(
			goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
			goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
			goleak.IgnoreTopFunction("github.com/stperic/zzrouter/pkg/cluster/mesh.(*HealthMonitor).monitorLoop"),
			goleak.IgnoreTopFunction("github.com/stperic/zzrouter/pkg/cluster/node.(*PairingStore).gcLoop"),
		); err != nil {
			fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// teardownSuite runs after all tests in the package complete.
// Cluster.Teardown is idempotent so this is safe even when the suite
// never provisioned (early-fail path).
func teardownSuite() {
	if suiteCancel != nil {
		suiteCancel()
	}
	if suiteCluster != nil {
		_ = suiteCluster.Teardown(context.Background())
	}
}

// suiteCluster + suiteRoutes are package-level — provisioned once via
// suiteOnce + setupSuite, torn down via TestMain ordering.
//
// One cluster + one catalog fetch shared across every Test* in the
// package. The previous design (per-test provision) ran 4 cluster
// boots per `go test` invocation; consolidation cuts cold-start ~3x.
var (
	suiteOnce    sync.Once
	suiteCluster *harness.Cluster
	suiteRoutes  []harness.RouteSpec
	suiteCancel  context.CancelFunc
)

func suite(t *testing.T) (*harness.Cluster, []harness.RouteSpec) {
	t.Helper()
	suiteOnce.Do(func() {
		t.Setenv("ZZROUTER_ADMIN_API_KEY", server.TestAdminKey)
		t.Setenv("ZZROUTER_API_KEY", server.TestUserKey)
		t.Setenv("ZZROUTER_CLUSTER_NETWORK_KEY", server.TestClusterKey)

		cfg := harness.Local()
		cluster, err := harness.New(cfg)
		if err != nil {
			t.Fatalf("harness.New: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second) //nolint:gosec // TestMain cancels the suite provisioning context after all tests.
		suiteCancel = cancel
		if err := cluster.Provision(ctx); err != nil {
			t.Fatalf("Provision: %v", err)
		}
		coord := cluster.Coordinator()
		cat, err := harness.FetchRoutes(ctx, coord.HTTPClient(), coord.BaseURL(), coord.AdminKey())
		if err != nil {
			t.Fatalf("FetchRoutes: %v", err)
		}
		suiteCluster = cluster
		suiteRoutes = cat.Routes
		// Teardown is invoked from TestMain after m.Run() so it doesn't
		// fire when the first calling test completes (which would tear
		// down the cluster before later parallel subtests connect).
	})
	return suiteCluster, suiteRoutes
}

// TestRing1_RouteCatalog_NonEmpty proves the harness can fetch the
// catalog. Smoke test — must pass before any subtest below has a
// chance.
func TestRing1_RouteCatalog_NonEmpty(t *testing.T) {
	_, routes := suite(t)
	if len(routes) == 0 {
		t.Fatal("empty route catalog")
	}
	t.Logf("catalog: %d routes", len(routes))
}

// slowProbeRoutes hit real network/hardware and have no finite-time
// completion guarantee. Excluded from the matrix tests until Ring 2+
// where we can mock or budget them properly.
var slowProbeRoutes = map[string]bool{
	"/zzrouter/v1/discover":               true,
	"/zzrouter/v1/discover/hardware":      true,
	"/zzrouter/v1/discover/hardware/gpus": true,
	"/zzrouter/v1/discover/network/hosts": true,
	"/zzrouter/v1/cluster/connect":        true,
	"/zzrouter/v1/cluster/validate":       true,
}

// authMatrixCandidate decides whether a route belongs in the auth
// matrix. Skips routes whose auth-without-key behavior the matrix
// can't assert without a fixture (path params, streaming, internal-
// dispatch-only, optional/none auth, slow network probes).
func authMatrixCandidate(r harness.RouteSpec) bool {
	if r.Auth == "none" || r.Auth == "optional" {
		return false
	}
	if strings.Contains(r.Path, ":") {
		return false
	}
	if r.Streaming != "" {
		return false
	}
	if r.Surface == "internal" {
		return false
	}
	if r.Method != http.MethodGet {
		// POST/PATCH/DELETE without auth would also 401, but they need
		// a body to test correctly; defer to the mutation matrix.
		return false
	}
	if slowProbeRoutes[r.Path] {
		return false
	}
	return true
}

// TestRing1_RetiredRoutesAnswerGone drives every route the catalog
// flags `retired` and asserts the server agrees.
//
// The flag is what lets an agent tell a gravestone from a live route
// without spending a request, so an unverified flag is worse than no
// flag: it would be believed. This is also the only place the harness
// reads the field off the wire, which is why the count is asserted —
// a RouteSpec that forgot to mirror it would make this pass on zero
// routes.
//
// The internal twins are excluded: internalRequestOnlyMiddleware 404s
// any HTTP caller by design, so driving them would test that gate.
func TestRing1_RetiredRoutesAnswerGone(t *testing.T) {
	cluster, routes := suite(t)
	coord := cluster.Coordinator()
	admin := harness.NewClient(coord, harness.TierAdmin)

	count := 0
	for _, r := range routes {
		if !r.Retired || r.Surface == "internal" {
			continue
		}
		count++
		r := r
		t.Run(r.Method+" "+r.Path, func(t *testing.T) {
			t.Parallel()
			d := assert.Diagnostics(t, cluster)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			// The handler reads no parameters; a placeholder just has to
			// occupy the segment. Nothing is mutated either way — the
			// route refuses before it does any work.
			path := fillPathParams(r.Path, "ring1-retired")
			resp, err := driveByMethod(ctx, admin, r.Method, path)
			if err != nil {
				t.Fatalf("%s %s: %v", r.Method, path, err)
			}
			d.RecordRequest(&harness.RequestRecord{Method: r.Method, URL: coord.BaseURL() + path})
			d.RecordResponse(&resp)

			if resp.Status != http.StatusGone {
				t.Fatalf("catalog says retired, server answered %d", resp.Status)
			}
			p, err := resp.Problem()
			if err != nil {
				t.Fatalf("410 body is not a problem+json: %v", err)
			}
			if p.Code != "retired" {
				t.Errorf("code = %q, want \"retired\": a caller branching on the code finds nothing", p.Code)
			}
		})
	}
	if count == 0 {
		t.Fatal("the catalog flagged no retired routes: either the server stopped " +
			"marking them or harness.RouteSpec dropped the field, and this gate is blind")
	}
	t.Logf("retired routes verified: %d", count)
}

// fillPathParams replaces gin's :param and *catchall segments with a
// fixed placeholder.
func fillPathParams(tmpl, placeholder string) string {
	segs := strings.Split(tmpl, "/")
	for i, seg := range segs {
		if len(seg) > 1 && (seg[0] == ':' || seg[0] == '*') {
			segs[i] = placeholder
		}
	}
	return strings.Join(segs, "/")
}

// driveByMethod sends the catalog's method without a body: a retired
// route refuses before reading one.
func driveByMethod(ctx context.Context, c *harness.Client, method, path string) (harness.Response, error) {
	switch method {
	case http.MethodPut:
		return c.PUT(ctx, path, nil)
	case http.MethodPatch:
		return c.PATCH(ctx, path, nil)
	case http.MethodDelete:
		return c.DELETE(ctx, path)
	case http.MethodPost:
		return c.POST(ctx, path, nil)
	default:
		return c.GET(ctx, path)
	}
}

// TestRing1_AuthMatrix walks the catalog and asserts the 401-when-no-key
// rule for admin + cluster-tier routes that satisfy authMatrixCandidate.
func TestRing1_AuthMatrix(t *testing.T) {
	cluster, routes := suite(t)
	coord := cluster.Coordinator()
	noAuth := harness.NewClient(coord, harness.TierNone)

	count := 0
	for _, r := range routes {
		if !authMatrixCandidate(r) {
			continue
		}
		count++
		r := r
		t.Run(r.Method+" "+r.Path, func(t *testing.T) {
			t.Parallel()
			d := assert.Diagnostics(t, cluster)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			resp, err := noAuth.GET(ctx, r.Path)
			if err != nil {
				t.Fatalf("GET %s: %v", r.Path, err)
			}
			d.RecordRequest(&harness.RequestRecord{Method: r.Method, URL: coord.BaseURL() + r.Path})
			d.RecordResponse(&resp)
			if resp.Status != http.StatusUnauthorized && resp.Status != http.StatusForbidden {
				t.Errorf("want 401/403 without auth, got %d (auth=%s)", resp.Status, r.Auth)
			}
		})
	}
	t.Logf("auth-matrix exercised: %d routes (%d in catalog)", count, len(routes))
}

// TestRing1_AuthMatrix_FalsePositiveImmunity proves the auth-matrix
// assertion catches a credential bug. A meta-test: stand up a fake
// always-200 handler in front of a "private" path and assert the
// matrix flags it. Without this the gate is correct-by-default — green
// when broken.
func TestRing1_AuthMatrix_FalsePositiveImmunity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // BUG: returns 200 without checking auth
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// Unrecorded: this handler is a prop for the assertion below, and
	// without the opt-out its 200 was captured as a real example of
	// GET /zzrouter/v1/keys and published in docs/api_examples.md.
	n, err := harness.NewNode("fake", srv.URL, harness.RoleCoordinator, nil,
		harness.NodeKeys{Admin: "should-not-matter"}, harness.Unrecorded())
	if err != nil {
		t.Fatal(err)
	}
	c := harness.NewClient(n, harness.TierNone)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.GET(ctx, "/zzrouter/v1/keys")
	if err != nil {
		t.Fatal(err)
	}
	// The matrix predicate (resp.Status == 401 || 403) MUST reject this.
	if resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden {
		t.Fatalf("matrix predicate accepted a leaky handler: status=%d", resp.Status)
	}
}

// TestRing1_HealthIsAnonymous walks every health route in the catalog
// and asserts each is reachable without a key.
func TestRing1_HealthIsAnonymous(t *testing.T) {
	cluster, routes := suite(t)
	coord := cluster.Coordinator()
	c := harness.NewClient(coord, harness.TierNone)

	count := 0
	for _, r := range routes {
		if r.Surface != "health" || r.Method != http.MethodGet {
			continue
		}
		count++
		r := r
		t.Run(r.Path, func(t *testing.T) {
			t.Parallel()
			d := assert.Diagnostics(t, cluster)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			resp, err := c.GET(ctx, r.Path)
			if err != nil {
				t.Fatal(err)
			}
			d.RecordRequest(&harness.RequestRecord{Method: r.Method, URL: coord.BaseURL() + r.Path})
			d.RecordResponse(&resp)
			// Health endpoints may legitimately return 503 during shutdown
			// or partial-readiness, but the contract is "no auth required."
			// Anything 2xx-5xx is fine; 401/403 specifically would fail.
			if resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden {
				t.Errorf("health endpoint required auth: status=%d body=%s", resp.Status, resp.Body)
			}
		})
	}
	if count == 0 {
		t.Fatal("no health routes in catalog")
	}
	t.Logf("health-anonymous exercised: %d routes", count)
}

// TestRing1_RequestIDHeader walks the same auth-matrix candidates with
// a valid admin key and asserts X-Request-Id is echoed on every
// response. Operator traceability depends on it.
func TestRing1_RequestIDHeader(t *testing.T) {
	cluster, routes := suite(t)
	coord := cluster.Coordinator()
	c := harness.NewClient(coord, harness.TierAdmin)

	count := 0
	for _, r := range routes {
		if !authMatrixCandidate(r) {
			continue
		}
		count++
		r := r
		t.Run(r.Method+" "+r.Path, func(t *testing.T) {
			t.Parallel()
			d := assert.Diagnostics(t, cluster)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			resp, err := c.GET(ctx, r.Path)
			if err != nil {
				t.Fatal(err)
			}
			d.RecordRequest(&harness.RequestRecord{Method: r.Method, URL: coord.BaseURL() + r.Path})
			d.RecordResponse(&resp)
			if resp.RequestID == "" {
				t.Errorf("X-Request-Id missing (status=%d)", resp.Status)
			}
		})
	}
	t.Logf("request-id exercised: %d routes", count)
}

// TestRing1_RolesAreDistinct proves the inproc backend boots coord +
// worker with the correct cluster modes — without this assertion a
// regression that flips both nodes to disabled mode would silently
// pass Ring 1 (the tests above only hit the coord).
func TestRing1_RolesAreDistinct(t *testing.T) {
	cluster, _ := suite(t)
	if cluster.Coordinator().Role() != harness.RoleCoordinator {
		t.Errorf("coordinator role: got %q", cluster.Coordinator().Role())
	}
	workers := cluster.Workers()
	if len(workers) == 0 {
		t.Fatal("no workers provisioned")
	}
	for _, w := range workers {
		if w.Role() != harness.RoleWorker {
			t.Errorf("worker %s role: got %q", w.Name(), w.Role())
		}
	}
}
