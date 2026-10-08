package mesh

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestConnector_DiscoverVersion_PrefersClusterURL pins the post-
// af3e7214 invariant: when the Connector is configured with a
// ClusterPort + mTLS-bound HTTPClient, version discovery must
// probe the cluster URL via that client BEFORE falling back to the
// admin URL via plain HTTP.
//
// Without this, attemptConnection's Step 1 hits the admin URL —
// loopback-only on workers — and the connect chain dies before
// performHealthCheck (Step 3) ever gets to do its mTLS probe.
func TestConnector_DiscoverVersion_PrefersClusterURL(t *testing.T) {
	t.Parallel()

	// "Cluster" server returns a healthy version response.
	clusterHits := 0
	cluster := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zzrouter/v1/internal/version" {
			http.NotFound(w, r)
			return
		}
		clusterHits++
		// Mirror handleGetVersion (response_helpers.go:217) which
		// emits version.VersionInfo directly without an envelope.
		// Version is a struct {major,minor,patch}, not a string.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":              map[string]any{"major": 0, "minor": 1, "patch": 1},
			"api_version":          "v1",
			"service_type":         "zzrouter-host",
			"cluster_protocol":     1,
			"min_cluster_protocol": 1,
			"capabilities":         []string{"cluster"},
		})
	}))
	defer cluster.Close()

	// "Admin" server records hits but should NEVER be reached when
	// ClusterPort is wired correctly.
	adminHits := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		adminHits++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer admin.Close()

	// Configure the connector to use the cluster server's port for
	// the mTLS-aware probe + insecure-skip-verify so httptest's
	// self-signed TLS doesn't trip the handshake.
	clusterPort := mustPort(t, cluster.URL)
	mtlsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only
		},
	}
	c := NewConnector(ConnectorConfig{
		HTTPClient:  mtlsClient,
		ClusterPort: clusterPort,
	})

	vi, err := c.discoverVersion(context.Background(), admin.URL)
	if err != nil {
		t.Fatalf("discoverVersion: %v", err)
	}
	if vi.ServiceType != "zzrouter-host" {
		t.Errorf("ServiceType=%q want zzrouter-host", vi.ServiceType)
	}
	if vi.ClusterProtocol != 1 {
		t.Errorf("ClusterProtocol=%d want 1", vi.ClusterProtocol)
	}
	if clusterHits != 1 {
		t.Errorf("cluster server hit count=%d want 1", clusterHits)
	}
	if adminHits != 0 {
		t.Errorf("admin server should not be hit, got %d", adminHits)
	}
}

// TestConnector_DiscoverVersion_FallsBackToAdmin pins the legacy /
// no-mTLS path: with ClusterPort==0, version discovery must hit
// the admin URL via the plain-HTTP cache (the existing
// version.VersionDiscovery instance). This keeps standalone /
// pre-pair coords working unchanged.
func TestConnector_DiscoverVersion_FallsBackToAdmin(t *testing.T) {
	t.Parallel()
	hits := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":          "0.1.1",
			"service_type":     "zzrouter-host",
			"cluster_protocol": 1,
		})
	}))
	defer admin.Close()

	c := NewConnector(ConnectorConfig{}) // no ClusterPort
	vi, err := c.discoverVersion(context.Background(), admin.URL)
	if err != nil {
		t.Fatalf("discoverVersion: %v", err)
	}
	if vi.ServiceType != "zzrouter-host" {
		t.Errorf("ServiceType=%q want zzrouter-host", vi.ServiceType)
	}
	if hits == 0 {
		t.Error("admin server was not probed")
	}
}

// TestConnector_DiscoverVersion_ClusterFailFallsBackToAdmin pins the
// failure-mode path: cluster URL probe fails (e.g. mTLS handshake
// rejected) → fall back to admin URL via the plain-HTTP cache so
// the connect chain can still surface a useful error from
// CheckCompatibility further down.
func TestConnector_DiscoverVersion_ClusterFailFallsBackToAdmin(t *testing.T) {
	t.Parallel()
	// Cluster server hangs up immediately.
	cluster := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer cluster.Close()

	adminHits := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/health") {
			http.NotFound(w, r)
			return
		}
		adminHits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":          "0.1.1",
			"service_type":     "zzrouter-host",
			"cluster_protocol": 1,
		})
	}))
	defer admin.Close()

	c := NewConnector(ConnectorConfig{
		HTTPClient: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only
		}},
		ClusterPort: mustPort(t, cluster.URL),
	})
	vi, err := c.discoverVersion(context.Background(), admin.URL)
	if err != nil {
		t.Fatalf("discoverVersion: %v", err)
	}
	if vi.ServiceType != "zzrouter-host" {
		t.Errorf("ServiceType=%q want zzrouter-host", vi.ServiceType)
	}
	if adminHits == 0 {
		t.Error("admin fallback was not exercised after cluster probe failed")
	}
}

func mustPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port %s: %v", u.Port(), err)
	}
	return p
}
