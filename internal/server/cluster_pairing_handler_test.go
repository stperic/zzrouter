package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// coordTestServer returns a test coordinator Server with the pairing
// admin routes wired up AND the cluster listener started. The
// listener bind is required by coordinatorSelfHostPort — the admin
// accept handler returns 503 "cluster listener not ready" otherwise,
// which is correct production behavior but prevents unit testing of
// the 200/404/410/409 paths. Port is OS-assigned so tests run in
// parallel.
func coordTestServer(t *testing.T) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := createTestNode(t, TestNodeConfig{
		AdminKey:    TestAdminKey,
		ClusterMode: pkgConfig.ClusterModeCoordinator,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = s.cluster.listener.Stop(context.Background()) })
	require.NoError(t, s.cluster.listener.Start(ctx))
	return s
}

// postAccept executes POST /zzrouter/v1/cluster/pairing/accept with
// the given code.
func postAccept(t *testing.T, s *Server, code string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"code": code})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/cluster/pairing/accept", bytes.NewReader(body))
	req.Header.Set("X-API-Key", TestAdminKey)
	req.Header.Set("Content-Type", "application/json")
	s.engine.ServeHTTP(w, req)
	return w
}

// seedPending records a pairing request on the test server's cluster
// listener so handler-level tests can drive the flow without the
// coord-side /cluster/pairing-request handler.
func seedPending(t *testing.T, s *Server, code, fp, name string) {
	t.Helper()
	csr, err := s.cluster.listener.BuildTestCSR(name)
	if err != nil {
		t.Fatalf("BuildTestCSR: %v", err)
	}
	_, err = s.cluster.listener.RecordPairingForTest(&clusternode.PairingRequest{
		Code:        code,
		Fingerprint: fp,
		NodeName:    name,
		CSRPEM:      csr,
	})
	require.NoError(t, err)
}

func TestClusterPairingAccept_HappyPath(t *testing.T) {
	s := coordTestServer(t)
	seedPending(t, s, "ACCEPTCODEABCDEF", "sha256:aaa", "worker-happy")

	w := postAccept(t, s, "ACCEPTCODEABCDEF")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var out pairingAcceptResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "worker-happy", out.NodeName)
	assert.Equal(t, "sha256:aaa", out.Fingerprint)
	// Short form is derived from fingerprint; "sha256:aaa" has hex
	// suffix "aaa" which is < 16 chars, so ShortForm returns "" —
	// test asserts we don't crash on the malformed-for-shortform case.
	_ = out.ShortForm
}

func TestClusterPairingAccept_UnknownCode(t *testing.T) {
	s := coordTestServer(t)

	// 16 chars, valid base32 alphabet, but never recorded on the store.
	w := postAccept(t, s, "UNKNOWNCODEABCDE")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestClusterPairingAccept_BadCodeFormat(t *testing.T) {
	s := coordTestServer(t)

	// Lowercase letters aren't in the base32 alphabet.
	w := postAccept(t, s, "lowercasecode!!!")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "malformed pairing code")
}

func TestClusterPairingAccept_MissingBody(t *testing.T) {
	s := coordTestServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/cluster/pairing/accept", nil)
	req.Header.Set("X-API-Key", TestAdminKey)
	s.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestClusterPairingAccept_NoAdminKey(t *testing.T) {
	s := coordTestServer(t)

	body, _ := json.Marshal(map[string]string{"code": "ACCEPTCODEABCDEF"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/cluster/pairing/accept", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.engine.ServeHTTP(w, req)

	// auth middleware rejects before handler runs.
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestClusterPairingList_Empty(t *testing.T) {
	s := coordTestServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/zzrouter/v1/cluster/pairing/pending", nil)
	req.Header.Set("X-API-Key", TestAdminKey)
	s.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var out struct {
		Pending []pairingPendingEntry `json:"pending"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Empty(t, out.Pending)
}

func TestClusterPairingList_Populated(t *testing.T) {
	s := coordTestServer(t)
	seedPending(t, s, "LISTCODEABCDEFGH", "sha256:bbb", "worker-list")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/zzrouter/v1/cluster/pairing/pending", nil)
	req.Header.Set("X-API-Key", TestAdminKey)
	s.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var out struct {
		Pending []pairingPendingEntry `json:"pending"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.Pending, 1)
	assert.Equal(t, "worker-list", out.Pending[0].NodeName)
	assert.Equal(t, "sha256:bbb", out.Pending[0].Fingerprint)
	// Code is deliberately NOT in the response shape — the field
	// doesn't exist on pairingPendingEntry. The struct definition is
	// the compile-time guarantee of redaction.
}

// Coverage for the format validator has moved to pkg/cluster/node's
// pairing_lifecycle_test (TestIsValidPairingCode). The single source
// of truth now lives there as clusternode.IsValidPairingCode; this
// handler delegates to it.

// TestClusterPairingAccept_UsesAdvertiseURLWhenSet pins that an
// operator-configured advertise_url overrides the derived-from-bind
// fallback. Without this, deployments where the bind address isn't
// worker-reachable (reverse proxy, NAT, 0.0.0.0 bind with no
// resolvable hostname) ship a broken coordinator_url to workers on
// accept, which silently breaks all future renewal dials.
func TestClusterPairingAccept_UsesAdvertiseURLWhenSet(t *testing.T) {
	s := coordTestServer(t)
	s.clusterHandlers.advertiseURL = "https://coord.example.com:9091"

	seedPending(t, s, "ADVERTCODEABCDEF", "sha256:adv", "worker-adv")

	// Start a WaitFor goroutine that catches the approved result the
	// handler stashes on the pending entry. Runs before postAccept so
	// it's parked on the done channel when Accept fires.
	resultCh := make(chan *clusternode.PairingResult, 1)
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r, err := s.cluster.listener.WaitForPairingResultForTest(ctx, "ADVERTCODEABCDEF")
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- r
	}()

	// Small yield so the goroutine parks before Accept fires.
	time.Sleep(10 * time.Millisecond)

	w := postAccept(t, s, "ADVERTCODEABCDEF")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	select {
	case r := <-resultCh:
		assert.Equal(t, "https://coord.example.com:9091", r.CoordinatorURL,
			"advertise_url must win over derived-from-bind")
	case err := <-errCh:
		t.Fatalf("WaitFor errored: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor did not return")
	}
}

// TestClusterPairingAccept_FailsWhenAdvertiseURLMalformed pins that
// a malformed advertise_url produces 503 at accept time. Defense-in-
// depth — Server Start also validates, but hot-reload / direct
// mutation paths could reintroduce malformed values.
func TestClusterPairingAccept_FailsWhenAdvertiseURLMalformed(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"missing scheme", "coord.example.com:9091"},
		{"missing host", "https://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := coordTestServer(t)
			s.clusterHandlers.advertiseURL = tc.url
			seedPending(t, s, "BADURLCODEABCDEF", "sha256:bad", "worker-bad")

			w := postAccept(t, s, "BADURLCODEABCDEF")
			assert.Equal(t, http.StatusServiceUnavailable, w.Code,
				"malformed advertise_url must surface as 503 at accept")
			assert.Contains(t, w.Body.String(), "advertise_url")
		})
	}
}

// TestClusterPairingAccept_FallsBackToBindWhenUnset pins the
// pre-advertise_url behavior: when the field is empty the coord
// derives the URL from listener.Addr() + bind host, keeping the
// simple loopback / single-host deploy working without config.
func TestClusterPairingAccept_FallsBackToBindWhenUnset(t *testing.T) {
	s := coordTestServer(t)
	// Explicitly empty so the test intent is clear.
	s.clusterHandlers.advertiseURL = ""

	seedPending(t, s, "FALLBKCODEABCDEF", "sha256:fb", "worker-fb")

	resultCh := make(chan *clusternode.PairingResult, 1)
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r, err := s.cluster.listener.WaitForPairingResultForTest(ctx, "FALLBKCODEABCDEF")
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- r
	}()
	time.Sleep(10 * time.Millisecond)

	w := postAccept(t, s, "FALLBKCODEABCDEF")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	select {
	case r := <-resultCh:
		// The cluster port is always mTLS — the derived URL MUST
		// start with "https://". F1 bug: using admin-port scheme
		// produced http:// on coords with admin-TLS off, silently
		// breaking worker renewal dials.
		assert.Regexp(t, `^https://[^/]+:\d+$`, r.CoordinatorURL,
			"derived coord URL must be https (cluster port is always mTLS)")
	case err := <-errCh:
		t.Fatalf("WaitFor errored: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor did not return")
	}
}
