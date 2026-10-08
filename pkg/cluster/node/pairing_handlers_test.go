package clusternode

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/version"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// coordForPairingHandlers builds a minimal *Node with a real CA +
// identity + pairing store wired up, enough to exercise
// handlePairingRequest and handleCAFingerprint without going through
// listener.go's full registration path. Uses a long TTL so tests don't
// race the GC goroutine.
func coordForPairingHandlers(t *testing.T) *Node {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		Mode:        Coordinator,
		IdentityDir: dir + "/identity",
		CADir:       dir + "/ca",
		NodeName:    "coord-test",
	}
	n, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		if n.pairingStore != nil {
			n.pairingStore.Stop()
		}
	})
	return n
}

// engineWithPairing builds a gin engine with the pairing routes
// mounted. We bypass listener.go's mode-gated registration so the
// tests can hit the handler directly; mode gating is tested separately
// in cluster_mode_gate_test.go / node_test.go.
func engineWithPairing(n *Node) *gin.Engine {
	e := gin.New()
	g := e.Group("/cluster")
	g.POST("/pairing-request",
		pairingRateLimitMiddleware(n.pairingRateLimiter),
		n.handlePairingRequest)
	g.GET("/ca-fingerprint", n.handleCAFingerprint)
	return e
}

func TestPairingRequestHandler_HappyPath(t *testing.T) {
	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	// Spawn Accept from a goroutine so the long-poll returns approved.
	done := make(chan struct{})
	go func() {
		// Give the handler a moment to park on WaitFor.
		for i := 0; i < 200; i++ {
			if n.pairingStore.inspect("HAPPYCODEABCDEFG") != nil {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		// Construct a CSR the CA will actually sign. Reuse the node's
		// own identity CSR so enforceCSRPublicKey passes.
		_, err := n.AcceptPairing("HAPPYCODEABCDEFG", "https://coord:9091")
		if err != nil {
			t.Errorf("AcceptPairing: %v", err)
		}
		close(done)
	}()

	csr, err := n.identity.CSR("worker-happy",
		[]string{"worker-happy.local"},
		nil,
	)
	require.NoError(t, err)

	body, _ := json.Marshal(pairingRequestBody{
		ClusterProtocol: version.ClusterProtocolVersion,
		NodeName:        "worker-happy",
		Fingerprint:     "sha256:deadbeef",
		CSRPEM:          string(csr),
		SANs:            []string{"worker-happy.local"},
		Code:            "HAPPYCODEABCDEFG",
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	<-done
	require.Equal(t, http.StatusOK, w.Code)
	var out pairingApprovedResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "approved", out.Status)
	assert.NotEmpty(t, out.SignedCertPEM)
	assert.Equal(t, version.Current.String(), out.CoordinatorBuildVersion,
		"approval must echo the coordinator's build version so the worker can log any mismatch")
	assert.Equal(t, version.ClusterProtocolVersion, out.ClusterProtocol,
		"approval must advertise the coordinator's cluster_protocol")
	assert.Equal(t, version.MinClusterProtocolVersion, out.MinClusterProtocol,
		"approval must advertise the coordinator's min_cluster_protocol")
	assert.NotEmpty(t, out.CACertPEM)
	assert.Equal(t, "https://coord:9091", out.CoordinatorURL)
}

func TestPairingRequestHandler_MalformedBody(t *testing.T) {
	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request",
		bytes.NewReader([]byte("not-json")))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestPairingRequestHandler_MissingClusterProtocol pins that a worker
// that doesn't advertise cluster_protocol (zero value) is rejected. No
// silent accept of pre-protocol builds.
func TestPairingRequestHandler_MissingClusterProtocol(t *testing.T) {
	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	// Raw map so cluster_protocol is omitted entirely; a struct literal
	// with a zero int would also trigger the gate, but a peer that
	// strips the field is the more realistic pre-protocol case.
	body, _ := json.Marshal(map[string]any{
		"node_name":   "w",
		"fingerprint": "fp",
		"csr_pem":     "x",
		"sans":        []string{},
		"code":        "NOPROTOCODEABCDE",
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "pre-protocol build")
	assert.Empty(t, n.PendingPairings(),
		"missing cluster_protocol must not poison the pending-request store")
}

// TestPairingRequestHandler_PeerAboveCeiling pins that a worker
// advertising a cluster_protocol newer than the coordinator understands
// is rejected. One-direction gating is not coordinator-blind.
func TestPairingRequestHandler_PeerAboveCeiling(t *testing.T) {
	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	body, _ := json.Marshal(pairingRequestBody{
		ClusterProtocol: version.ClusterProtocolVersion + 5,
		NodeName:        "w",
		Fingerprint:     "fp-future",
		CSRPEM:          "---",
		Code:            "FUTURECODEABCDEF",
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "upgrade the coordinator")
	assert.Empty(t, n.PendingPairings())
}

func TestPairingRequestHandler_FingerprintConflict(t *testing.T) {
	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	// Pre-seed a pending entry with fingerprint X, code A.
	_, err := n.pairingStore.Record(&PairingRequest{
		Code:        "FIRSTCODEABCDEFG",
		Fingerprint: "fp-conflict",
		NodeName:    "w",
		CSRPEM:      "---",
	})
	require.NoError(t, err)

	body, _ := json.Marshal(pairingRequestBody{
		ClusterProtocol: version.ClusterProtocolVersion,
		NodeName:        "w",
		Fingerprint:     "fp-conflict",
		CSRPEM:          "---",
		Code:            "SECONDCODEXXYYZZ",
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestPairingRequestHandler_LongPollTimeout(t *testing.T) {
	// Override the long-poll hold via a custom test engine: shorten by
	// calling WaitFor with a tighter ctx. The cleanest way is to use
	// the real handler but drop the hold constant via a test-only
	// package-level override. We don't have one, so exercise the
	// timeout path indirectly: use a very short TTL so the entry
	// expires BEFORE pairingLongPollHold, and assert the expired
	// response shape.
	//
	// This doubles as coverage for the expiry branch in the handler.
	n := coordForPairingHandlers(t)
	// Swap in a short-TTL store. Stop the existing one first.
	n.pairingStore.Stop()
	n.pairingStore = NewPairingStore(80 * time.Millisecond)
	engine := engineWithPairing(n)

	body, _ := json.Marshal(pairingRequestBody{
		ClusterProtocol: version.ClusterProtocolVersion,
		NodeName:        "w",
		Fingerprint:     "fp-expire",
		CSRPEM:          "---",
		Code:            "EXPIRECODEABCDEF",
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var out pairingExpiredResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "expired", out.Status)
}

// TestPairingRequestHandler_StrictModeRejectsTOFU pins the coord-side
// gate: when RequireSecurePairing is set, a pair request whose
// coordinator_ca_fingerprint is empty or mismatched must be rejected
// with 403 before it reaches the pairing store.
func TestPairingRequestHandler_StrictModeRejectsTOFU(t *testing.T) {
	n := coordForPairingHandlers(t)
	n.cfg.RequireSecurePairing = true
	engine := engineWithPairing(n)

	csr, err := n.identity.CSR("worker-strict",
		[]string{"worker-strict.local"},
		nil,
	)
	require.NoError(t, err)

	t.Run("empty fingerprint rejected", func(t *testing.T) {
		body, _ := json.Marshal(pairingRequestBody{
			ClusterProtocol: version.ClusterProtocolVersion,
			NodeName:        "worker-strict",
			Fingerprint:     "sha256:deadbeef",
			CSRPEM:          string(csr),
			Code:            "STRICTCODEABCDEF",
			// no CoordinatorCAFingerprint
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Contains(t, w.Body.String(), "rejects TOFU")
	})

	t.Run("mismatched fingerprint rejected", func(t *testing.T) {
		body, _ := json.Marshal(pairingRequestBody{
			ClusterProtocol:          version.ClusterProtocolVersion,
			NodeName:                 "worker-strict",
			Fingerprint:              "sha256:deadbeef",
			CSRPEM:                   string(csr),
			Code:                     "STRICTCODEABCDEF",
			CoordinatorCAFingerprint: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Contains(t, w.Body.String(), "does not match")
	})
}

// TestCAFingerprintHandler_RequireSecurePairing pins that the
// preflight response surfaces the RequireSecurePairing config bit so
// worker-side preflights can fail-closed before opening a window.
func TestCAFingerprintHandler_RequireSecurePairing(t *testing.T) {
	n := coordForPairingHandlers(t)
	n.cfg.RequireSecurePairing = true
	engine := engineWithPairing(n)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/cluster/ca-fingerprint", nil)
	engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var out caFingerprintResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.True(t, out.RequireSecurePairing, "strict coord must advertise require_secure_pairing=true")
}

func TestPairingRequestHandler_RateLimit(t *testing.T) {
	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	// Rate-limit runs BEFORE the handler body, so a malformed JSON
	// body still consumes tokens and returns 400 until the bucket
	// empties, at which point we get 429. Using malformed bodies
	// avoids blocking in the long-poll on calls that make it past
	// the middleware.
	body := []byte("not-json-so-handler-returns-400-fast")

	statuses := make([]int, 0, 20)
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		engine.ServeHTTP(w, req)
		statuses = append(statuses, w.Code)
	}
	// Per-IP burst is 5 tokens; after that the bucket empties and we
	// hit 429 while the global bucket still has room.
	var got429 bool
	for _, s := range statuses {
		if s == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	assert.True(t, got429, "expected 429 after per-IP burst; saw statuses %v", statuses)
}

func TestCAFingerprintHandler(t *testing.T) {
	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/cluster/ca-fingerprint", nil)
	engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var out caFingerprintResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.True(t, strings.HasPrefix(out.Fingerprint, "sha256:"))
	assert.Contains(t, out.ShortForm, "…")
	// CAFingerprint accessor agrees.
	assert.Equal(t, n.CAFingerprint(), out.Fingerprint)
}

func TestAcceptPairing_Unknown(t *testing.T) {
	n := coordForPairingHandlers(t)
	_, err := n.AcceptPairing("NONEXISTENTCODE!", "https://coord")
	// format-invalid codes are filtered by the admin handler; at the
	// Node layer we just see ErrPairingCodeUnknown.
	assert.ErrorIs(t, err, ErrPairingCodeUnknown)
}

func TestAcceptPairing_NonCoordinator(t *testing.T) {
	// Build a Disabled-mode node (no CA, no store).
	cfg := Config{Mode: Disabled}
	n, err := New(cfg)
	require.NoError(t, err)
	_, err = n.AcceptPairing("ANYCODE1234ABCDE", "x")
	assert.ErrorIs(t, err, ErrNotCoordinator)
}

func TestPendingPairings_Snapshot(t *testing.T) {
	n := coordForPairingHandlers(t)

	_, err := n.pairingStore.Record(&PairingRequest{
		Code:        "SNAPCODEABCDEFGH",
		Fingerprint: "fp-snap",
		NodeName:    "worker-snap",
		CSRPEM:      "---",
	})
	require.NoError(t, err)

	pending := n.PendingPairings()
	require.Len(t, pending, 1)
	assert.Equal(t, "worker-snap", pending[0].NodeName)
	assert.Equal(t, "fp-snap", pending[0].Fingerprint)
}

// TestPairingRequestHandler_RejectsOversizedBody pins the H2 fix:
// unauthenticated POST bodies larger than pairingRequestMaxBodyBytes
// (128KB) must trip MaxBytesReader during the JSON decode and return
// 400, not allocate 200KB of attacker-controlled bytes.
func TestPairingRequestHandler_RejectsOversizedBody(t *testing.T) {
	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	// 200KB body, shaped as a plausible JSON prefix so the MaxBytes
	// trip fires inside the JSON decoder (not before it starts).
	oversized := make([]byte, 200*1024)
	copy(oversized, `{"pairing_version":1,"code":"`)
	// Fill the middle with A's to pad well beyond the cap.
	for i := len(`{"pairing_version":1,"code":"`); i < len(oversized)-2; i++ {
		oversized[i] = 'A'
	}
	oversized[len(oversized)-2] = '"'
	oversized[len(oversized)-1] = '}'

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code,
		"oversized body must return 400, got %d body=%s", w.Code, w.Body.String())
}

// TestPairingRequestHandler_RejectsMalformedCode pins M1: a worker
// POSTing a malformed code must be rejected at the handler BEFORE
// the store records a pending entry. Otherwise the store fills with
// entries no admin Accept can ever match.
func TestPairingRequestHandler_RejectsMalformedCode(t *testing.T) {
	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	body, _ := json.Marshal(pairingRequestBody{
		ClusterProtocol: version.ClusterProtocolVersion,
		NodeName:        "w",
		Fingerprint:     "fp-malformed",
		CSRPEM:          "---",
		Code:            "lowercase-is-bad", // 16 chars but lowercase
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "malformed pairing code")

	// Store must NOT have recorded the poisoned entry.
	assert.Empty(t, n.PendingPairings(),
		"malformed code must not poison the pending-request store")
}

// TestPairingRequestHandler_RejectsPeerBelowMinProtocol pins the
// protocol gate's below-floor branch end-to-end: a worker advertising
// cluster_protocol < coordinator.Min is rejected before the CSR reaches
// the store, and the response body carries the three protocol numbers
// so the operator can tell which side to upgrade.
//
// Tightens the window via the pairingProtocolWindow test seam rather
// than mutating version package consts.
func TestPairingRequestHandler_RejectsPeerBelowMinProtocol(t *testing.T) {
	orig := pairingProtocolWindow
	t.Cleanup(func() { pairingProtocolWindow = orig })
	pairingProtocolWindow = func() version.ProtocolWindow {
		return version.ProtocolWindow{Min: 2, Max: 2}
	}

	n := coordForPairingHandlers(t)
	engine := engineWithPairing(n)

	body, _ := json.Marshal(pairingRequestBody{
		ClusterProtocol: 1,
		NodeName:        "old-worker",
		Fingerprint:     "fp-old",
		CSRPEM:          "---",
		Code:            "OLDWORKERPAIRABC",
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cluster/pairing-request", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "upgrade the peer")
	assert.Contains(t, w.Body.String(), `"coordinator_protocol":2`)
	assert.Contains(t, w.Body.String(), `"coordinator_min":2`)
	assert.Contains(t, w.Body.String(), `"peer_protocol":1`)
	assert.Empty(t, n.PendingPairings(),
		"below-min-protocol worker must not poison the pending-request store")
}
