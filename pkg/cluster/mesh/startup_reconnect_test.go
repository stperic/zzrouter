package mesh

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stperic/zzrouter/pkg/version"
)

// fakePeer stands up a TLS server that answers the two cluster-port
// probes attemptConnection makes — version discovery and the internal
// health check — well enough to drive an endpoint to StatusUp. It
// returns the server plus a Connector wired to probe it at full
// quality (QualityFull requires the cluster-port probe to succeed;
// the public /health fallback only ever yields QualityDegraded).
func fakePeer(t *testing.T, nodeName string) (*httptest.Server, *Connector) {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zzrouter/v1/internal/version":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version":              map[string]any{"major": 0, "minor": 1, "patch": 1},
				"api_version":          "v1",
				"service_type":         version.ServiceTypeHost,
				"cluster_protocol":     version.ClusterProtocolVersion,
				"min_cluster_protocol": version.MinClusterProtocolVersion,
				"capabilities":         version.ClusterRequiredCapabilities,
			})
		case "/zzrouter/v1/internal/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":       "healthy",
				"hostname":     nodeName,
				"cluster_role": "worker",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	connector := NewConnector(ConnectorConfig{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only, httptest self-signed
			},
		},
		ClusterPort: mustPort(t, srv.URL),
	})
	return srv, connector
}

// recorder collects reconnection callbacks.
type recorder struct {
	mu   sync.Mutex
	urls []string
}

func (r *recorder) callback(hostURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, hostURL)
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.urls...)
}

// A peer that is already up when this node starts transitions
// Unknown->UP inside NewCluster. That is the transition the
// reconnection callback exists for, and before ReplayStartupReconnects
// it was dropped on the floor: the callback fired for every recovery
// except the first one, so anything hung off it (today the model-cache
// refresh and the provider-tree reconcile) silently skipped boot.
func TestNewCluster_StartupUpIsDeliveredOnReplay(t *testing.T) {
	t.Parallel()

	srv, connector := fakePeer(t, "w1")
	var rec recorder

	cluster, err := NewCluster(&Config{
		Enabled:   true,
		Endpoints: []string{srv.URL},
		BindPort:  mustPort(t, srv.URL),
	}, "http://localhost:9090", &MockLocalHandler{}, rec.callback, connector)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	defer cluster.Stop()

	// The probe must have driven the endpoint up -- otherwise the rest
	// of this test would pass for the wrong reason.
	ep, err := cluster.GetState().GetEndpoints().GetEndpointByURL(srv.URL)
	if err != nil {
		t.Fatalf("endpoint not registered: %v", err)
	}
	if ep.Status != StatusUp {
		t.Fatalf("seed probe left endpoint %s, want %s", ep.Status, StatusUp)
	}

	// Inline delivery is the one thing the constructor must not do:
	// the caller has no handle on this cluster yet.
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("callback fired during construction: %v", got)
	}

	cluster.ReplayStartupReconnects()
	if got := rec.seen(); len(got) != 1 || got[0] != srv.URL {
		t.Fatalf("replay delivered %v, want exactly [%s]", got, srv.URL)
	}

	// Idempotent: a second replay must not re-fire work that already ran.
	cluster.ReplayStartupReconnects()
	if got := rec.seen(); len(got) != 1 {
		t.Fatalf("second replay re-delivered: %v", got)
	}
}

// The mirror case: a peer that was NOT up at boot has no transition to
// replay. Its recovery is the health monitor's job, and delivering a
// reconnect for it here would announce a peer that never connected.
func TestNewCluster_UnreachablePeerHasNothingToReplay(t *testing.T) {
	t.Parallel()

	// A server closed before use gives us a port that refuses
	// connections immediately, so the probe fails fast.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	var rec recorder
	cluster, err := NewCluster(&Config{
		Enabled:   true,
		Endpoints: []string{deadURL},
		BindPort:  mustPort(t, deadURL),
	}, "http://localhost:9090", &MockLocalHandler{}, rec.callback, nil)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	defer cluster.Stop()

	cluster.ReplayStartupReconnects()
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("replayed a reconnect for a peer that never connected: %v", got)
	}
}

// A cluster built without a callback must not panic on replay, and
// must still clear what it held — the nil-callback path is how every
// worker and every degraded-mode caller constructs a cluster.
func TestReplayStartupReconnects_SafeWithoutACallback(t *testing.T) {
	t.Parallel()

	srv, connector := fakePeer(t, "w1")
	cluster, err := NewCluster(&Config{
		Enabled:   true,
		Endpoints: []string{srv.URL},
		BindPort:  mustPort(t, srv.URL),
	}, "http://localhost:9090", &MockLocalHandler{}, nil, connector)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	defer cluster.Stop()

	cluster.ReplayStartupReconnects()

	cluster.startupMu.Lock()
	held := len(cluster.startupUp)
	cluster.startupMu.Unlock()
	if held != 0 {
		t.Errorf("replay left %d transition(s) held with no callback to run", held)
	}
}
