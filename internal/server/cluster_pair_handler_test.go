package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

const (
	testCoordURL = "https://coord:9091"
	testCoordCA  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// unclaimedTestServer returns a test server in Unclaimed mode (the
// node boots with ClusterMode=worker, no prior pairing state on disk,
// so buildClusternodeConfig resolves to Unclaimed). Pairing inputs
// come from the request body — they are no longer persisted in
// cluster config.
func unclaimedTestServer(t *testing.T) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return createTestNode(t, TestNodeConfig{
		AdminKey:    TestAdminKey,
		ClusterMode: pkgConfig.ClusterModeWorker,
	})
}

// pairBody is the full request body shape including the new pairing
// inputs. Tests that only want a subset can still pass a partial body
// — the JSON decoder tolerates missing fields.
type pairBody struct {
	Regenerate               bool   `json:"regenerate,omitempty"`
	Cancel                   bool   `json:"cancel,omitempty"`
	CoordinatorURL           string `json:"coordinator_url,omitempty"`
	CoordinatorCAFingerprint string `json:"coordinator_ca_fingerprint,omitempty"`
}

// defaultPairBody returns a body pre-populated with valid pairing
// inputs — the common case for tests that exercise the handshake
// rather than the rejection paths.
func defaultPairBody() pairBody {
	return pairBody{
		CoordinatorURL:           testCoordURL,
		CoordinatorCAFingerprint: testCoordCA,
	}
}

// postPair sends a POST /zzrouter/v1/cluster/pair request and returns
// the recorder.
func postPair(t *testing.T, s *Server, body pairBody) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/cluster/pair", bytes.NewReader(b))
	req.Header.Set("X-API-Key", TestAdminKey)
	req.Header.Set("Content-Type", "application/json")
	s.engine.ServeHTTP(w, req)
	return w
}

func TestClusterPair_NewWindow(t *testing.T) {
	s := unclaimedTestServer(t)

	w := postPair(t, s, defaultPairBody())
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var out clusterPairResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "new", out.Status)
	assert.Len(t, out.Code, 16)
	assert.False(t, out.Deadline.IsZero())
}

func TestClusterPair_Idempotent(t *testing.T) {
	s := unclaimedTestServer(t)

	first := postPair(t, s, defaultPairBody())
	require.Equal(t, http.StatusOK, first.Code)
	var firstOut clusterPairResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstOut))

	second := postPair(t, s, defaultPairBody())
	require.Equal(t, http.StatusOK, second.Code)
	var secondOut clusterPairResponse
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &secondOut))

	assert.Equal(t, "existing", secondOut.Status)
	assert.Equal(t, firstOut.Code, secondOut.Code)
}

func TestClusterPair_Regenerate(t *testing.T) {
	s := unclaimedTestServer(t)

	first := postPair(t, s, defaultPairBody())
	var firstOut clusterPairResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstOut))

	regenBody := defaultPairBody()
	regenBody.Regenerate = true
	second := postPair(t, s, regenBody)
	var secondOut clusterPairResponse
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &secondOut))

	assert.Equal(t, "new", secondOut.Status)
	assert.NotEqual(t, firstOut.Code, secondOut.Code)
}

func TestClusterPair_CancelWithoutWindow(t *testing.T) {
	s := unclaimedTestServer(t)

	w := postPair(t, s, pairBody{Cancel: true})
	require.Equal(t, http.StatusOK, w.Code)
	var out clusterPairResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "no_active_window", out.Status)
}

func TestClusterPair_CancelActiveWindow(t *testing.T) {
	s := unclaimedTestServer(t)

	// Open a window.
	first := postPair(t, s, defaultPairBody())
	require.Equal(t, http.StatusOK, first.Code)

	// Cancel — no coord fields needed, just the cancel flag.
	w := postPair(t, s, pairBody{Cancel: true})
	require.Equal(t, http.StatusOK, w.Code)
	var out clusterPairResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "cancelled", out.Status)
}

// TestClusterPair_ReuseOmitsFields pins the doc-comment contract on
// clusterPairRequest: "ignored on reuse calls against an already-
// active window." After a window opens, a subsequent call with no
// coord fields must succeed by reusing the window rather than 400-ing
// on missing fields — that's what lets a TUI poll the handler for
// status at 1 Hz without re-sending pairing inputs every tick.
func TestClusterPair_ReuseOmitsFields(t *testing.T) {
	s := unclaimedTestServer(t)

	// Open a window with full pairing inputs.
	first := postPair(t, s, defaultPairBody())
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())
	var firstOut clusterPairResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstOut))
	require.Equal(t, "new", firstOut.Status)

	// Second call with no coord fields — must reuse the active window
	// without a 400 even though the request body is bare.
	reuse := postPair(t, s, pairBody{})
	require.Equal(t, http.StatusOK, reuse.Code, "reuse must not 400; body=%s", reuse.Body.String())
	var reuseOut clusterPairResponse
	require.NoError(t, json.Unmarshal(reuse.Body.Bytes(), &reuseOut))
	assert.Equal(t, "existing", reuseOut.Status)
	assert.Equal(t, firstOut.Code, reuseOut.Code)
}

// TestClusterPair_EmptyBodyIsStatusPoll asserts an empty-body POST
// with no active window returns no_active_window + the runtime mode,
// not a 400. The TUI wizard polls at 1 Hz with an empty body and
// infers success from Mode=="worker" after the coord consumes the
// window — a 400 here would strand the client in stateConfirmRepair
// post-success.
func TestClusterPair_EmptyBodyIsStatusPoll(t *testing.T) {
	s := unclaimedTestServer(t)

	w := postPair(t, s, pairBody{})
	assert.Equal(t, http.StatusOK, w.Code)
	var out clusterPairResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "no_active_window", out.Status)
	assert.Equal(t, "unclaimed", out.Mode)
}

func TestClusterPair_RejectsCoordinatorMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := createTestNode(t, TestNodeConfig{
		AdminKey:    TestAdminKey,
		ClusterMode: pkgConfig.ClusterModeCoordinator,
	})

	w := postPair(t, s, defaultPairBody())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "unclaimed")
}

func TestClusterPair_NoAdminKey(t *testing.T) {
	s := unclaimedTestServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/cluster/pair", nil)
	s.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
